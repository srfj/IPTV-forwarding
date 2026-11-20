package zerotier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/iptv-zt-bridge/internal/config"
)

// PeerDetail 来自 /peer API 的详细连接信息（ZeroTier 1.6+）
type PeerDetail struct {
	Address  string `json:"address"`  // peer 的 ZeroTier 短地址
	Role     string `json:"role"`     // LEAF/RELAY/PLANET
	Latency  int    `json:"latency"`  // ms，<=0 通常表示"未连通"
	Tunneled bool   `json:"tunneled"` // true=走 TCP 中继，false=UDP 直连
	Active   bool   `json:"active"`   // 是否存在至少一个 active 且未过期的 path
	Paths    string `json:"paths"`    // path 概要，如 "192.168.0.210/9993(active)"
}

// Status ZeroTier 状态
type Status struct {
	Running           bool         `json:"running"`
	NetworkID         string       `json:"network_id"`
	MemberAddress     string       `json:"member_address"`
	PhysicalAddress   string       `json:"physical_address"`
	VirtualAddresses  []string     `json:"virtual_addresses"`
	NetworkOnline     bool         `json:"network_online"`
	NetworkConfigured bool         `json:"network_configured"`
	TunMode           bool         `json:"tun_mode"`
	TunError          string       `json:"tun_error,omitempty"`
	Relay             string       `json:"relay,omitempty"`
	FallbackUsed      bool         `json:"fallback_used,omitempty"`
	Peers             int          `json:"peers"`
	ActivePeers       int          `json:"active_peers"` // 有有效连接的 peer 数（latency>0 且有未过期 path）
	PeerDetails       []PeerDetail `json:"peer_details,omitempty"`
	LastCheck         time.Time    `json:"last_check"`
	LastError         string       `json:"last_error,omitempty"`
	Version           string       `json:"version,omitempty"` // 内嵌 ZeroTier 版本（应为 1.14.2）

	// 隧道接口与路由自检（Open vSwitch / 容器环境下 ZeroTier 可能漏建）
	ZTInterface   string `json:"zt_interface,omitempty"`   // tun 接口名，如 ztfl6gcng5
	ZTMTU         int    `json:"zt_mtu,omitempty"`         // tun 接口当前 MTU（应 ≤1500）
	ZTMTUOK       bool   `json:"zt_mtu_ok,omitempty"`      // MTU 是否 ≤1500（避免 UDP 分片被丢弃）
	TunnelRoute   string `json:"tunnel_route,omitempty"`   // 内核里 tunnel 网段的路由条目（空=缺失）
	TunnelRouteOK bool   `json:"tunnel_route_ok,omitempty"` // connected route 是否存在

	// ZeroTier 网络配置与数据面自检（判断两端成员之间能否真正通信）
	BroadcastEnabled *bool `json:"broadcast_enabled,omitempty"` // 网络是否允许二层广播（ARP 依赖它）
	NetworkMtu       int   `json:"network_mtu,omitempty"`       // 网络配置的 MTU
	NetconfRevision  int   `json:"netconf_revision,omitempty"`  // 网络配置版本（两端一致才说明配置同步）
	PortError        int   `json:"port_error,omitempty"`        // ZeroTier UDP 端口错误码（0=正常）
	ActiveLeafPeers  int   `json:"active_leaf_peers,omitempty"` // 已建立活跃路径的非 PLANET peer 数
	TunneledPeers    int   `json:"tunneled_peers,omitempty"`    // 走中继(TCP relay)的 peer 数
}

// Peer 节点信息
type Peer struct {
	Address string `json:"address"`
	Role    string `json:"role"`
	Latency int    `json:"latency"`
}

// Manager ZeroTier 管理器
type Manager struct {
	cfg      *config.Config
	status   Status
	mu       sync.RWMutex
	proc     *os.Process
	cmd      *exec.Cmd
	stopping bool
	client   *http.Client
	pollOnce sync.Once

	// 用于识别启动即崩溃（参数错误/权限不足）并避免无限重启
	startedAt time.Time
	failCount int

	// 进程生命周期上下文（不受单次 HTTP 请求取消影响）
	baseCtx context.Context

	// 防抖：避免 MTU / 路由抖动时每次 poll 都刷日志
	lastMtuFix   time.Time
	lastRouteFix time.Time
}

// NewManager 创建 ZeroTier 管理器
func NewManager(cfg *config.Config) *Manager {
	return &Manager{
		cfg:     cfg,
		baseCtx: context.Background(),
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// Start 启动 ZeroTier 进程
func (m *Manager) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	if m.proc != nil {
		m.mu.Unlock()
		return nil
	}
	m.stopping = false
	m.mu.Unlock()

	ztCfg := m.cfg.ZeroTier

	if err := os.MkdirAll(ztCfg.DataDir, 0755); err != nil {
		return fmt.Errorf("create zerotier data dir: %w", err)
	}

	// moons.d/<id>.moon 是 ZeroTier 官方的“公网中继(moon)”加载目录。
	// 两个站点都在 NAT 后、UDP 打洞失败时，唯一可靠的连通方式就是让两端
	// 都 orbit 到一台有公网 IP 的中继节点；把该节点的 .moon 文件放进这里即可。
	moonsDir := filepath.Join(ztCfg.DataDir, "moons.d")
	_ = os.MkdirAll(moonsDir, 0755)
	if entries, err := os.ReadDir(moonsDir); err == nil {
		n := 0
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".moon") {
				n++
			}
		}
		if n > 0 {
			fmt.Printf("[zt] 已发现 %d 个 moon 中继配置（%s），ZeroTier 启动时自动加载\n", n, moonsDir)
		}
	}

	if _, err := os.Stat(ztCfg.BinaryPath); err != nil {
		return fmt.Errorf("zerotier binary not found at %s: %w", ztCfg.BinaryPath, err)
	}

	// 先清理残留的 zerotier-one：飞牛套件升级、app 重启时旧进程可能还活着，
	// 新二进制会端口冲突或复用旧配置，local.conf 就白写了。
	m.killStaleZerotier()

	// ZeroTier 1.14.2 的参数格式：
	//   zerotier-one [-U] [-p<port>] [home directory]
	// 注意：不支持 -D 指定数据目录（须用位置参数），
	// 也不支持 --no-tap / --allow-gateway，传入会被判为非法参数直接退出。
	args := []string{
		"-U", // 以 root 运行时不降权，保证能创建/配置 tun 接口
		fmt.Sprintf("-p%d", ztCfg.APIPort),
		ztCfg.DataDir,
	}

	if ztCfg.APIToken != "" {
		tokenFile := filepath.Join(ztCfg.DataDir, "authtoken.secret")
		if err := os.WriteFile(tokenFile, []byte(ztCfg.APIToken), 0600); err != nil {
			return fmt.Errorf("write zerotier api token: %w", err)
		}
	}

	// 关键：宿主机装有 Docker（飞牛 fnOS 默认自带）时，ZeroTier 会把 docker0 / veth
	// 也当成可用出口，向对端发送的 UDP 被路由到 docker 网桥（表现为邻居表里
	// 出现 “<公网IP> dev docker0 INCOMPLETE”），导致与客户端无法建立路径。
	// 用 interfacePrefixBlacklist 排除容器网桥，只走真实物理网卡。
	m.writeLocalConf(ztCfg.DataDir, ztCfg.APIPort)

	tunEnabled := ztCfg.EnableTun
	fallback := false
	tunDetail := ""
	if tunEnabled {
		var ok bool
		ok, tunDetail = initTunDevice()
		if !ok {
			if ztCfg.FallbackUserspace {
				tunEnabled = false
				fallback = true
				fmt.Printf("[zt] warning: /dev/net/tun 不可用（%s），将以无虚拟接口模式运行，"+
					"此时无法访问对端 ZeroTier 虚拟 IP\n", tunDetail)
			} else {
				return fmt.Errorf("tun device init failed: %s", tunDetail)
			}
		}
	}

	cmd := exec.CommandContext(m.baseCtx, ztCfg.BinaryPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// 让 zerotier-one 与所有可能的 fork 子进程都归属于同一进程组，
	// 这样 Stop 时能用负 PID 一次性清干净（主进程退出后子进程还活着会占端口 9993）。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start zerotier: %w", err)
	}

	m.mu.Lock()
	m.cmd = cmd
	m.proc = cmd.Process
	m.startedAt = time.Now()
	m.status.Running = true
	m.status.LastCheck = time.Now()
	m.status.TunMode = tunEnabled
	m.status.TunError = tunDetail
	m.status.FallbackUsed = fallback
	m.mu.Unlock()

	go m.waitAndRestart(m.baseCtx, cmd)

	// 等待进程就绪后加入网络
	time.Sleep(2 * time.Second)

	m.mu.RLock()
	alive := m.proc != nil
	lastErr := m.status.LastError
	m.mu.RUnlock()
	if !alive {
		if lastErr == "" {
			lastErr = "进程已退出，请查看套件日志"
		}
		return fmt.Errorf("ZeroTier 启动失败：%s", lastErr)
	}

	if ztCfg.NetworkID != "" {
		// 本地 API 端口可能尚未就绪，重试加入网络
		var joinErr error
		for i := 0; i < 5; i++ {
			if joinErr = m.JoinNetwork(ztCfg.NetworkID); joinErr == nil {
				break
			}
			time.Sleep(2 * time.Second)
		}
		if joinErr != nil {
			fmt.Fprintf(os.Stderr, "[zt] join network warning: %v\n", joinErr)
		}
	}

	// 让 tun 接口起来 + 等内核分配好 IP，然后立刻补建 connected route / MTU，
	// 不要等 pollStatus 的 10 秒轮询。
	time.Sleep(5 * time.Second)
	m.updateStatus()
	m.logIdentity()
	m.ensureTunnelIntegrity()

	m.pollOnce.Do(func() { go m.pollStatus(m.baseCtx) })

	return nil
}

// logIdentity 打印并持久化本机 ZeroTier 成员地址。
// 同一数据目录下成员地址若发生变化，说明 identity.secret 没有被保留
// （每次重启都会以“新成员”身份入网，控制台里会不断出现待授权的新节点）。
func (m *Manager) logIdentity() {
	st := m.Status()
	if st.MemberAddress == "" {
		return
	}
	path := filepath.Join(m.cfg.ZeroTier.DataDir, "last_member")
	prev, _ := os.ReadFile(path)
	prevAddr := strings.TrimSpace(string(prev))

	fmt.Printf("[zt] 身份 member=%s datadir=%s\n", st.MemberAddress, m.cfg.ZeroTier.DataDir)
	if prevAddr != "" && prevAddr != st.MemberAddress {
		fmt.Fprintf(os.Stderr,
			"[zt] 警告：ZeroTier 身份已变化 %s → %s（identity 未持久化，需在控制台重新授权该成员）\n",
			prevAddr, st.MemberAddress)
	}
	_ = os.WriteFile(path, []byte(st.MemberAddress), 0600)
}

// Stop 停止 ZeroTier 进程组，确保主进程与 fork 出的所有子进程一起被清干净，
// 避免残留进程继续占 9993 端口导致下次 Start 失败或复用了旧 local.conf。
func (m *Manager) Stop() error {
	m.mu.Lock()
	m.stopping = true
	proc := m.proc
	m.proc = nil
	m.cmd = nil
	m.status.Running = false
	m.status.NetworkOnline = false
	m.mu.Unlock()

	if proc == nil {
		return nil
	}

	pid := proc.Pid

	// 先温和地 SIGTERM 整个进程组
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	time.Sleep(300 * time.Millisecond)

	// 如果还活着，SIGKILL 进程组兜底
	if _, err := proc.Wait(); err == nil {
		return nil
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)

	// 保险起见，pkill 残留的 zerotier-one（不管属于哪个父进程）
	out, _ := exec.Command("pgrep", "-f", "zerotier-one").CombinedOutput()
	for _, p := range strings.Fields(string(out)) {
		_ = exec.Command("kill", "-KILL", p).Run()
	}

	return nil
}

// waitAndRestart 等待进程退出，异常退出时自动重启
func (m *Manager) waitAndRestart(ctx context.Context, cmd *exec.Cmd) {
	err := cmd.Wait()

	m.mu.Lock()
	m.proc = nil
	m.cmd = nil
	m.status.Running = false
	m.status.NetworkOnline = false
	stopping := m.stopping
	rapid := time.Since(m.startedAt) < 10*time.Second
	if rapid {
		m.failCount++
	} else {
		m.failCount = 0
	}
	fails := m.failCount
	if err != nil {
		m.status.LastError = fmt.Sprintf("进程退出: %v", err)
	}
	m.mu.Unlock()

	fmt.Fprintf(os.Stderr, "[zt] process exited: %v\n", err)
	if stopping || ctx.Err() != nil {
		return
	}

	// 连续快速失败（参数错误、权限不足等）时停止重试，避免死循环
	if fails >= 3 {
		m.mu.Lock()
		m.status.LastError = fmt.Sprintf("连续 %d 次启动失败，已停止重试: %v", fails, err)
		m.mu.Unlock()
		fmt.Fprintf(os.Stderr, "[zt] 连续 %d 次启动失败，停止重试\n", fails)
		return
	}

	select {
	case <-ctx.Done():
		return
	case <-time.After(3 * time.Second):
	}

	fmt.Println("[zt] restarting...")
	if err := m.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "[zt] restart failed: %v\n", err)
	}
}

// pollStatus 轮询 ZeroTier API 获取状态，并在每次轮询后自检 tunnel 接口、
// connected route 与 MTU（Open vSwitch / 容器环境下 ZeroTier 可能漏建路由或继承物理网卡 MTU）。
func (m *Manager) pollStatus(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	m.updateStatus()
	m.ensureTunnelIntegrity()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.updateStatus()
			m.ensureTunnelIntegrity()
		}
	}
}

// ensureTunnelIntegrity 组合调用 ensureZtRoute + ensureZtMtu，确保 ZeroTier
// tunnel 在各种宿主机环境下都可达。
func (m *Manager) ensureTunnelIntegrity() {
	if !m.Status().TunMode {
		return
	}
	m.ensureZtMtu()
	m.ensureZtRoute()
}

// localToken 返回本地 API 鉴权 token。
// 1.6.6 的本地 API 必须携带 X-ZT1-Auth，否则一律返回 401。
func (m *Manager) localToken() string {
	if m.cfg.ZeroTier.APIToken != "" {
		return m.cfg.ZeroTier.APIToken
	}
	b, err := os.ReadFile(filepath.Join(m.cfg.ZeroTier.DataDir, "authtoken.secret"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// doAPI 请求 ZeroTier 本地 API（自动附带鉴权头）
func (m *Manager) doAPI(method, path string, body io.Reader) (*http.Response, error) {
	base := fmt.Sprintf("http://127.0.0.1:%d", m.cfg.ZeroTier.APIPort)
	req, err := http.NewRequest(method, base+path, body)
	if err != nil {
		return nil, err
	}
	if tok := m.localToken(); tok != "" {
		req.Header.Set("X-ZT1-Auth", tok)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return m.client.Do(req)
}

// updateStatus 从本地 API 更新状态
func (m *Manager) updateStatus() {
	resp, err := m.doAPI("GET", "/status", nil)
	if err != nil {
		m.mu.Lock()
		m.status.Running = false
		m.status.NetworkOnline = false
		m.status.LastCheck = time.Now()
		m.mu.Unlock()
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		m.mu.Lock()
		m.status.Running = false
		m.mu.Unlock()
		return
	}

	body, _ := io.ReadAll(resp.Body)

	var apiStatus map[string]interface{}
	if err := json.Unmarshal(body, &apiStatus); err != nil {
		return
	}

	m.mu.Lock()
	m.status.Running = true
	m.status.LastCheck = time.Now()
	if addr, ok := apiStatus["address"].(string); ok {
		m.status.MemberAddress = addr
	}
	if phy, ok := apiStatus["physicalAddress"].(string); ok {
		m.status.PhysicalAddress = phy
	}
	if v, ok := apiStatus["version"].(string); ok {
		m.status.Version = v
	}
	m.mu.Unlock()

	// 通过 /network 获取网络详情（/status 不保证包含 networks）
	if m.cfg.ZeroTier.NetworkID != "" {
		netMap := m.fetchNetwork(m.cfg.ZeroTier.NetworkID)
		if netMap == nil {
			m.mu.Lock()
			m.status.NetworkOnline = false
			m.status.NetworkConfigured = false
			m.status.LastError = "无法读取网络状态（本地 API 不可用或尚未加入网络）"
			m.mu.Unlock()
		} else {
			// 1.6.6 的 /network/<id> 不提供 online 字段，
			// 以 status == "OK"（或已分配虚拟 IP）作为在线判据。
			statusStr, _ := netMap["status"].(string)
			var ips []string
			if raw, ok := netMap["assignedAddresses"].([]interface{}); ok {
				for _, ip := range raw {
					if ipStr, ok := ip.(string); ok {
						ips = append(ips, ipStr)
					}
				}
			}

			online := statusStr == "OK" || len(ips) > 0
			if _, hasStatus := netMap["status"]; !hasStatus {
				if v, ok := netMap["online"].(bool); ok {
					online = v
				}
			}

			// 采集决定“两端成员能否互通”的关键网络配置：
			// - broadcastEnabled=false 时 ZeroTier 不转发 ARP 广播，同网络成员永远互相不可达；
			// - netconfRevision 两端一致才说明拿到了同一份网络配置；
			// - portError≠0 表示本机 UDP 端口绑定异常。
			var broadcast *bool
			if v, ok := netMap["broadcastEnabled"].(bool); ok {
				broadcast = &v
			}
			var netMtu, netRev, portErr int
			if v, ok := netMap["mtu"].(float64); ok {
				netMtu = int(v)
			}
			if v, ok := netMap["netconfRevision"].(float64); ok {
				netRev = int(v)
			}
			if v, ok := netMap["portError"].(float64); ok {
				portErr = int(v)
			}

			m.mu.Lock()
			m.status.NetworkOnline = online
			m.status.NetworkConfigured = online && len(ips) > 0
			m.status.VirtualAddresses = ips
			m.status.BroadcastEnabled = broadcast
			m.status.NetworkMtu = netMtu
			m.status.NetconfRevision = netRev
			m.status.PortError = portErr
			switch statusStr {
			case "ACCESS_DENIED":
				m.status.LastError = "节点未授权：请在 my.zerotier.com 勾选 Authorized"
			case "NOT_FOUND":
				m.status.LastError = "网络不存在或尚未加入"
			case "REQUESTING_CONFIGURATION":
				m.status.LastError = "正在向网络控制器请求配置…"
			default:
				m.status.LastError = ""
			}
			m.mu.Unlock()
		}
	}

	// 获取 peers（含详细连接状态）
	if resp2, err := m.doAPI("GET", "/peer", nil); err == nil {
		defer resp2.Body.Close()
		if body2, err := io.ReadAll(resp2.Body); err == nil {
			var peers []map[string]interface{}
			if json.Unmarshal(body2, &peers) == nil {
				details := make([]PeerDetail, 0, len(peers))
				activeCount := 0
				activeLeaf := 0
				tunneled := 0
				for _, p := range peers {
					d := PeerDetail{
						Address: mapStr(p, "address"),
						Role:    mapStr(p, "role"),
					}
					if lat, ok := p["latency"].(float64); ok {
						d.Latency = int(lat)
					}
					if t, ok := p["tunneled"].(bool); ok {
						d.Tunneled = t
					}
					// paths
					if rawPaths, ok := p["paths"].([]interface{}); ok {
						var paths []string
						anyActive := false
						for _, raw := range rawPaths {
							pm, ok := raw.(map[string]interface{})
							if !ok {
								continue
							}
							addr := mapStr(pm, "address")
							active, _ := pm["active"].(bool)
							expired, _ := pm["expired"].(bool)
							if active && !expired {
								anyActive = true
							}
							tag := ""
							switch {
							case active && !expired:
								tag = "(active)"
							case active:
								tag = "(expired)"
							case expired:
								tag = "(expired)"
							default:
								tag = "(inactive)"
							}
							paths = append(paths, addr+tag)
						}
						d.Paths = strings.Join(paths, ",")
						d.Active = anyActive
					}
					if d.Active && d.Latency >= 0 {
						activeCount++
					}
					// 统计“非 PLANET 成员(LEAF)”中已建立活跃路径的数量：
					// 它对端必须能从 PLANET 之外直接/中继连上我们，才说明成员间数据面可用。
					if d.Active && d.Role != "PLANET" && d.Role != "" {
						activeLeaf++
					}
					if d.Tunneled {
						tunneled++
					}
					details = append(details, d)
				}
				m.mu.Lock()
				m.status.Peers = len(peers)
				m.status.ActivePeers = activeCount
				m.status.PeerDetails = details
				m.status.ActiveLeafPeers = activeLeaf
				m.status.TunneledPeers = tunneled
				m.mu.Unlock()
			}
		}
	}

	// tunnel 接口 / 路由 / MTU 自检（只读，不调用 ensure 函数）
	m.refreshTunnelDiagnostics()
}

// refreshTunnelDiagnostics 只读采集 tunnel 接口名、MTU、connected route，
// 写入 status 字段（供 WebUI / 诊断页面直接显示，确认路由补建是否生效）。
// MTU 从 sysfs /sys/class/net/<iface>/mtu 读，比 net.Interfaces().MTU 更实时。
func (m *Manager) refreshTunnelDiagnostics() {
	if !m.Status().TunMode {
		return
	}
	iface := m.findZtIfaceNameOnly()

	mtu := 0
	if iface != "" {
		m, err := readIfMtuSysfs(iface)
		if err == nil {
			mtu = m
		}
	}
	// MTU ≤1500 才能保证 ZeroTier 的 UDP 封装包在常见路径上不被分片丢弃。
	mtuOK := mtu > 0 && mtu <= 1500

	routeOK := false
	routeText := ""
	if iface != "" && len(m.Status().VirtualAddresses) > 0 {
		_, ipNet, err := net.ParseCIDR(m.Status().VirtualAddresses[0])
		if err == nil {
			out, _ := exec.Command("ip", "route", "show", ipNet.String()).CombinedOutput()
			for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if ln != "" && strings.Contains(ln, "dev "+iface) {
					routeOK = true
					routeText = ln
					break
				}
			}
		}
	}

	m.mu.Lock()
	m.status.ZTInterface = iface
	m.status.ZTMTU = mtu
	m.status.ZTMTUOK = mtuOK
	m.status.TunnelRoute = routeText
	m.status.TunnelRouteOK = routeOK
	m.mu.Unlock()
}

// mapStr 从 map[string]interface{} 安全提取 string 字段
func mapStr(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// writeLocalConf 写入/合并 local.conf，排除容器网桥，并确保 UDP 打洞与中继可用。
// 合并已有配置，避免覆盖用户手工设置。
func (m *Manager) writeLocalConf(dataDir string, port int) {
	path := filepath.Join(dataDir, "local.conf")

	conf := map[string]interface{}{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &conf)
	}

	settings, _ := conf["settings"].(map[string]interface{})
	if settings == nil {
		settings = map[string]interface{}{}
	}

	// docker0 / veth / br-* 等都是容器网桥，不是真实出口；
	// ovs-system（群晖 OVS 父接口，永远 down）与 sit0（6to4 隧道）也不应作为出口。
	settings["interfacePrefixBlacklist"] = []string{
		"docker", "veth", "br-", "virbr", "lxc", "tap", "cni", "flannel", "kube",
		"ovs-system", "sit",
	}
	settings["primaryPort"] = port
	settings["portMappingEnabled"] = true
	settings["allowTcpFallbackRelay"] = true
	conf["settings"] = settings

	out, err := json.MarshalIndent(conf, "", "\t")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[zt] local.conf 生成失败: %v\n", err)
		return
	}
	if err := os.WriteFile(path, out, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "[zt] local.conf 写入失败: %v\n", err)
		return
	}
	fmt.Printf("[zt] local.conf 已写入：排除容器/OVS/tunnel 网桥，port=%d，portMapping+TCP-relay 已开启\n", port)
}

// killStaleZerotier 终止任何残留的 zerotier-one 进程，确保本次 Start 能干净地启动。
// 飞牛套件升级或 app 重启时旧进程可能还在跑，此时端口 9993 会被占用，
// 我们的新进程启动失败或复用旧 local.conf。
func (m *Manager) killStaleZerotier() {
	out, err := exec.Command("pgrep", "-f", "zerotier-one").CombinedOutput()
	if err != nil || len(out) == 0 {
		return
	}
	for _, pidStr := range strings.Fields(string(out)) {
		cmd := exec.Command("kill", "-TERM", pidStr)
		_ = cmd.Run()
	}
	time.Sleep(500 * time.Millisecond)
	// 还没死就 SIGKILL
	out2, _ := exec.Command("pgrep", "-f", "zerotier-one").CombinedOutput()
	if len(out2) > 0 {
		for _, pidStr := range strings.Fields(string(out2)) {
			cmd := exec.Command("kill", "-KILL", pidStr)
			_ = cmd.Run()
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Printf("[zt] 已清理残留 zerotier-one 进程：%s\n", strings.ReplaceAll(string(out), "\n", ","))
}

// fetchNetwork 查询单个网络信息
func (m *Manager) fetchNetwork(networkID string) map[string]interface{} {
	resp, err := m.doAPI("GET", "/network/"+networkID, nil)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var netMap map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&netMap); err != nil {
		return nil
	}
	return netMap
}

// Status 返回当前状态副本
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

// GetVirtualIP 获取 ZeroTier 虚拟 IP（IPv4）
func (m *Manager) GetVirtualIP() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, addr := range m.status.VirtualAddresses {
		ip, _, err := net.ParseCIDR(addr)
		if err == nil {
			ipv4 := ip.To4()
			if ipv4 != nil {
				return ipv4.String()
			}
		}
	}
	return ""
}

// JoinNetwork 请求加入 ZeroTier 网络
func (m *Manager) JoinNetwork(networkID string) error {
	resp, err := m.doAPI("POST", "/network/"+networkID, bytes.NewBufferString(`{}`))
	if err != nil {
		return fmt.Errorf("join network request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("join network failed: %s %s", resp.Status, string(respBody))
	}

	m.mu.Lock()
	m.status.NetworkID = networkID
	m.mu.Unlock()

	// 等待网络配置完成
	time.Sleep(3 * time.Second)
	m.updateStatus()

	return nil
}

// LeaveNetwork 离开网络
func (m *Manager) LeaveNetwork(networkID string) error {
	resp, err := m.doAPI("DELETE", "/network/"+networkID, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	m.updateStatus()
	return nil
}

// apiStop 通过 API 停止
func (m *Manager) apiStop() {
	_, _ = m.doAPI("GET", "/controller", nil)
}

// initTunDevice 初始化 tun 设备，返回 (是否可用, 失败详情)
func initTunDevice() (bool, string) {
	if tunUsable() {
		return true, ""
	}

	var notes []string

	// 1. 创建设备节点（部分 DSM 没有 /dev/net 目录）
	if err := os.MkdirAll("/dev/net", 0755); err != nil {
		notes = append(notes, "mkdir /dev/net: "+err.Error())
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		if err := syscall.Mknod("/dev/net/tun", syscall.S_IFCHR|0666, int((10<<8)|200)); err != nil {
			notes = append(notes, "mknod: "+err.Error())
		}
	}

	// 2. 加载 tun 内核模块（DSM 常缺 modules.dep，modprobe 会失败，需回退 insmod）
	if !tunUsable() {
		if msg := loadTunModule(); msg != "" {
			notes = append(notes, msg)
		}
	}

	// 3. 模块加载是异步的，重试探测
	for i := 0; i < 10; i++ {
		if tunUsable() {
			return true, ""
		}
		time.Sleep(300 * time.Millisecond)
	}

	if _, err := os.Stat("/dev/net/tun"); err != nil {
		notes = append(notes, "/dev/net/tun 不存在")
	} else if f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0); err != nil {
		notes = append(notes, "open: "+err.Error())
	} else {
		f.Close()
	}
	return false, strings.Join(notes, "; ")
}

// tunUsable 探测 /dev/net/tun 是否可打开（模块未加载时返回 ENODEV）
func tunUsable() bool {
	f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// loadTunModule 尝试用多种方式加载 tun 模块，返回失败说明（成功返回空）
func loadTunModule() string {
	var notes []string
	for _, p := range []string{"/sbin/modprobe", "/usr/sbin/modprobe", "modprobe"} {
		if _, err := os.Stat(p); err != nil && !strings.Contains(p, "/") {
			continue
		}
		if out, err := exec.Command(p, "tun").CombinedOutput(); err == nil {
			return ""
		} else if len(out) > 0 {
			notes = append(notes, "modprobe: "+strings.TrimSpace(string(out)))
		}
	}

	patterns := []string{
		"/lib/modules/tun.ko",
		"/lib/modules/*/tun.ko",
		"/lib/modules/*/kernel/drivers/net/tun.ko",
		"/usr/lib/modules/tun.ko",
		"/usr/lib/modules/*/kernel/drivers/net/tun.ko",
	}
	for _, pat := range patterns {
		files, _ := filepath.Glob(pat)
		for _, f := range files {
			if out, err := exec.Command("/sbin/insmod", f).CombinedOutput(); err == nil {
				return ""
			} else if len(out) > 0 {
				notes = append(notes, "insmod "+f+": "+strings.TrimSpace(string(out)))
			}
		}
	}
	if len(notes) == 0 {
		notes = append(notes, "未找到可用的 tun 内核模块")
	}
	return strings.Join(notes, "; ")
}

// TunnelTest 测试 ZeroTier 隧道连通性
func (m *Manager) TunnelTest(targetIP string) (bool, time.Duration) {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(targetIP, "22"), 3*time.Second)
	if err != nil {
		return false, 0
	}
	conn.Close()
	return true, time.Since(start)
}

// GetMembershipAuthHint 获取成员批准提示
func (m *Manager) GetMembershipAuthHint() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.status.MemberAddress == "" || m.status.NetworkID == "" {
		return "ZeroTier 未完全启动"
	}

	if !m.status.NetworkConfigured {
		return fmt.Sprintf("请访问 https://my.zerotier.com/network/%s 并批准节点 %s 加入",
			m.status.NetworkID, m.status.MemberAddress)
	}

	if !m.status.NetworkOnline {
		return "节点已加入但未在线，检查网络连接"
	}

	return "网络已就绪"
}

// findZtIface 通过 ZeroTier 虚拟 IP 找到对应的 tun 接口名（如 ztfl6gcng5）。
// 遍历所有 up 且非 loopback 的接口，找出那个匹配 virtual IP 的。
// ZeroTier tun 接口名通常以 "zt" 开头，本函数不依赖名字，只靠 IP 匹配。
func (m *Manager) findZtIface() (ifaceName string, mtu int) {
	vips := m.Status().VirtualAddresses
	if len(vips) == 0 {
		return "", 0
	}
	v4 := net.ParseIP(strings.Split(vips[0], "/")[0]).To4()
	if v4 == nil {
		return "", 0
	}

	ifs, err := net.Interfaces()
	if err != nil {
		return "", 0
	}
	for _, in := range ifs {
		if in.Flags&net.FlagUp == 0 || in.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := in.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil && ip4.Equal(v4) {
				return in.Name, in.MTU
			}
		}
	}
	return "", 0
}

// readIfMtuSysfs 从 /sys/class/net/<iface>/mtu 读取 MTU。
// 比 net.Interfaces().MTU 更可靠：后者可能是 ioctl 缓存，
// 某些内核/平台组合下 ZeroTier 虽然已把 MTU 重置但 Go 还读不到。
func readIfMtuSysfs(iface string) (int, error) {
	path := fmt.Sprintf("/sys/class/net/%s/mtu", iface)
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	mtu, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, err
	}
	return mtu, nil
}

// ensureZtMtu 把过大的 ZeroTier tun MTU 降到物理链路能承载的范围。
//
// 之前这里把 MTU 强行“统一”为 2800，方向是错的：ZeroTier 会把整个 L2 帧
// 封装进一个 UDP 报文，tun MTU=2800 就意味着它会发出 ~2850 字节的 UDP 包。
// 而两端真实出口（家宽/运营商 NAT）的路径 MTU 基本是 1500，这些超长包必须
// 靠 IP 分片，很多 NAT/防火墙会静默丢弃分片 → 打洞/握手包丢失 → 成员之间
// 始终建立不了路径（现象正是：客户端 MTU=1500 能看到全部 planet+leaf，
// 服务端被我们顶到 2800，只能连上 2 个 planet）。
//
// 修复：只在 MTU 超过 1500 时下调到 1400（留出 ZeroTier 封装头），
// 已经 ≤1500 的接口完全不碰，避免和内核/ZeroTier 互相“拉锯”造成刷屏。
func (m *Manager) ensureZtMtu() {
	iface := m.findZtIfaceNameOnly()
	if iface == "" {
		return
	}
	const maxSafe = 1500
	const target = 1400

	mtu, err := readIfMtuSysfs(iface)
	if err != nil {
		return
	}
	// 已经安全就不再干预：ZeroTier 自己维护的 1500 是正常的，越改越乱。
	if mtu <= maxSafe {
		return
	}

	if err := exec.Command("ip", "link", "set", iface, "mtu", strconv.Itoa(target)).Run(); err != nil {
		// 同时尝试直接写 sysfs，兜底某些精简 iproute 的环境
		_ = os.WriteFile(fmt.Sprintf("/sys/class/net/%s/mtu", iface), []byte(strconv.Itoa(target)), 0644)
	}

	// 防抖：30s 内只打一条日志
	if time.Since(m.lastMtuFix) > 30*time.Second {
		fmt.Printf("[zt] %s MTU=%d 超过物理链路 1500，下调为 %d（避免 UDP 分片被 NAT 丢弃）\n", iface, mtu, target)
		m.mu.Lock()
		m.lastMtuFix = time.Now()
		m.mu.Unlock()
	}
}

// findZtIfaceNameOnly 只返回 ZeroTier tun 接口名（不读 MTU，避免走 net.Interfaces 缓存）
func (m *Manager) findZtIfaceNameOnly() string {
	vips := m.Status().VirtualAddresses
	if len(vips) == 0 {
		return ""
	}
	v4 := net.ParseIP(strings.Split(vips[0], "/")[0]).To4()
	if v4 == nil {
		return ""
	}
	ifs, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, in := range ifs {
		if in.Flags&net.FlagUp == 0 || in.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := in.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil && ip4.Equal(v4) {
				return in.Name
			}
		}
	}
	return ""
}

// ensureZtRoute 确保 ZeroTier 虚拟网段存在 connected route。
// 某些内核/平台组合下（Open vSwitch、带 policy routing 的宿主机），
// ZeroTier tun 虽然被分配了 IP/掩码，但内核没有自动建立 connected route，
// 导致隧道内所有地址不可达（ip route 里看不到 10.x.x.0/24 dev ztxxx）。
func (m *Manager) ensureZtRoute() {
	vips := m.Status().VirtualAddresses
	if len(vips) == 0 {
		return
	}
	_, ipNet, err := net.ParseCIDR(vips[0])
	if err != nil {
		return
	}
	subnet := ipNet.String() // e.g. "10.219.200.0/24"

	iface := m.findZtIfaceNameOnly()
	if iface == "" {
		return
	}

	// 检查路由是否已存在且走正确的 tun 接口
	out, _ := exec.Command("ip", "route", "show", subnet).CombinedOutput()
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if ln != "" && strings.Contains(ln, "dev "+iface) {
			return // 已有有效路由
		}
	}

	// 先尝试清理（可能存在指向错误接口的同网段路由），然后 add
	_ = exec.Command("ip", "route", "del", subnet).Run()
	args := []string{"route", "add", subnet, "dev", iface, "scope", "link"}
	if err := exec.Command("ip", args...).Run(); err != nil {
		// 失败时再尝试不带 scope link（某些精简环境 iproute 行为不同）
		args2 := []string{"route", "add", subnet, "dev", iface}
		_ = exec.Command("ip", args2...).Run()
	}

	// 防抖：30s 内只打一条
	if time.Since(m.lastRouteFix) > 30*time.Second {
		fmt.Printf("[zt] 已补建 ZeroTier 路由：%s dev %s scope link\n", subnet, iface)
		m.mu.Lock()
		m.lastRouteFix = time.Now()
		m.mu.Unlock()
	}
}

// ZTBinaryURL ZeroTier 各架构二进制下载地址
var ZTBinaryURL = map[string]string{
	"x86_64": "https://download.zerotier.com/RELEASES/1.6.6/dist/linux/zerotier-one_x64",
	"armv7":  "https://download.zerotier.com/RELEASES/1.6.6/dist/linux/zerotier-one_armv7l",
	"arm64":  "https://download.zerotier.com/RELEASES/1.6.6/dist/linux/zerotier-one_arm64",
}

// DetectArch 检测当前架构
func DetectArch() string {
	out, err := exec.Command("uname", "-m").Output()
	if err != nil {
		return "x86_64"
	}
	arch := strings.TrimSpace(string(out))

	switch arch {
	case "x86_64", "amd64":
		return "x86_64"
	case "armv7l", "armv6l", "arm":
		return "armv7"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return "x86_64"
	}
}
