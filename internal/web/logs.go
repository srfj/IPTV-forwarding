package web

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iptv-zt-bridge/internal/config"
	"github.com/iptv-zt-bridge/internal/zerotier"
)

// apiLogs 返回运行日志尾部内容（含子进程输出），供 WebUI 直接复制
func (s *Server) apiLogs(w http.ResponseWriter, r *http.Request) {
	dir := s.cfg.Runtime.LogDir
	if dir == "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{"log": "（未配置日志目录）", "files": []string{}})
		return
	}

	files := []string{}
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if !e.IsDir() {
				files = append(files, e.Name())
			}
		}
		sort.Strings(files)
	}

	name := r.URL.Query().Get("file")
	if name == "" || strings.ContainsAny(name, "/\\") {
		name = "app.log"
	}

	path := filepath.Join(dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"log":   fmt.Sprintf("（读取 %s 失败：%v）", path, err),
			"files": files,
		})
		return
	}

	const maxLines = 1200
	lines := strings.Split(string(data), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"log":   strings.Join(lines, "\n"),
		"file":  name,
		"files": files,
	})
}

// apiDiagnostics 采集网络与监听状态，用于定位“客户端连不上服务端”
func (s *Server) apiDiagnostics(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder

	b.WriteString("=== 监听状态 ===\n")
	for _, addr := range []string{"127.0.0.1:8081", "127.0.0.1:8089"} {
		c, err := net.DialTimeout("tcp", addr, 800*time.Millisecond)
		if err != nil {
			fmt.Fprintf(&b, "%s  不可连接：%v\n", addr, err)
			continue
		}
		_ = c.Close()
		fmt.Fprintf(&b, "%s  可连接\n", addr)
	}

	b.WriteString("\n=== 网卡地址 ===\n")
	if ifs, err := net.Interfaces(); err == nil {
		for _, in := range ifs {
			addrs, _ := in.Addrs()
			list := make([]string, 0, len(addrs))
			for _, a := range addrs {
				list = append(list, a.String())
			}
			fmt.Fprintf(&b, "%s up=%v mtu=%d %s\n", in.Name, in.Flags&net.FlagUp != 0, in.MTU, strings.Join(list, " "))
		}
	}

	b.WriteString("\n=== ZeroTier ===\n")
	st := s.zt.Status()
	fmt.Fprintf(&b, "running=%v network=%s online=%v configured=%v tun=%v relay=%q peers=%d active_peers=%d\n",
		st.Running, st.NetworkID, st.NetworkOnline, st.NetworkConfigured, st.TunMode, st.Relay, st.Peers, st.ActivePeers)
	fmt.Fprintf(&b, "member=%s virtual=%v zerotier_version=%s\n", st.MemberAddress, st.VirtualAddresses, st.Version)
	if st.LastError != "" {
		fmt.Fprintf(&b, "last_error=%s\n", st.LastError)
	}
	if st.TunMode {
		iface := st.ZTInterface
		if iface == "" {
			iface = "（未找到 tun 接口，可能 ZeroTier 还未完全就绪）"
		}
		fmt.Fprintf(&b, "tun_interface=%s mtu=%d mtu_ok=%v\n", iface, st.ZTMTU, st.ZTMTUOK)
		if st.ZTMTU > 1500 {
			fmt.Fprintf(&b, "注意：MTU=%d 超过物理链路 1500，ZeroTier 的 UDP 封装包会被分片，\n"+
				"      很多 NAT/防火墙直接丢弃分片，导致成员之间建立不了路径（本程序会自动下调到 1400）\n", st.ZTMTU)
		}
		if st.TunnelRouteOK {
			fmt.Fprintf(&b, "tunnel_route=OK → %s\n", st.TunnelRoute)
		} else {
			fmt.Fprintf(&b, "tunnel_route=缺失（隧道内地址不可达！）\n")
		}
	}
	// 网络配置：直接决定同网络成员之间能否互通
	bcast := "未知(本地API未返回)"
	if st.BroadcastEnabled != nil {
		bcast = fmt.Sprintf("%v", *st.BroadcastEnabled)
	}
	fmt.Fprintf(&b, "broadcast_enabled=%s network_mtu=%d netconf_revision=%d port_error=%d\n",
		bcast, st.NetworkMtu, st.NetconfRevision, st.PortError)
	fmt.Fprintf(&b, "active_leaf_peers=%d tunneled_peers=%d\n", st.ActiveLeafPeers, st.TunneledPeers)
	if verdict := dataPlaneVerdict(st); verdict != "" {
		fmt.Fprintf(&b, "数据面判定：%s\n", verdict)
	}
	if len(st.PeerDetails) > 0 {
		b.WriteString("peers:\n")
		for _, p := range st.PeerDetails {
			qual := "无有效路径"
			switch {
			case !p.Active:
				qual = "无 active path"
			case p.Latency <= 0:
				qual = "latency<=0"
			case p.Tunneled:
				qual = fmt.Sprintf("OK(中继,tunnel) latency=%dms", p.Latency)
			default:
				qual = fmt.Sprintf("OK(直连) latency=%dms", p.Latency)
			}
			fmt.Fprintf(&b, "  %s role=%s %s paths=%s\n", p.Address, p.Role, qual, p.Paths)
		}
	}

	b.WriteString("\n=== 防火墙 (iptables) ===\n")
	b.WriteString(runBrief("iptables", "-S", "INPUT"))

	b.WriteString("\n=== 防火墙 (nftables) ===\n")
	b.WriteString(runBrief("nft", "list", "ruleset"))

	b.WriteString("\n=== 监听端口 ===\n")
	ssOut := runBrief("ss", "-lntp")
	filtered := filterLines(ssOut, "8081", "8089", "8086", "9993")
	b.WriteString(filtered)

	// 如果过滤后没有任何一行命中，把 ss 原始输出的前 30 行贴出来，方便排查
	if strings.TrimSpace(filtered) == "（无匹配监听）" {
		lines := strings.Split(ssOut, "\n")
		n := 30
		if len(lines) < n {
			n = len(lines)
		}
		b.WriteString("ss 原始输出前 30 行：\n")
		b.WriteString(strings.Join(lines[:n], "\n"))
	}

	// 服务端自己 curl 自己的 discover，验证 WebUI 本身没问题
	b.WriteString("\n=== 本地 discover 接口测试 ===\n")
	for _, url := range []string{
		"http://127.0.0.1:8089/api/discover",
	} {
		code, body := httpCheck(url, 2*time.Second)
		fmt.Fprintf(&b, "%s  → HTTP %d  body=%s\n", url, code, body)
	}

	// 如果 eth0 有 IPv4，也直接用 eth0 IP 测 discover（验证能否被局域网访问）
	if addrs, err := net.Interfaces(); err == nil {
		for _, in := range addrs {
			if in.Name != "eth0" || in.Flags&net.FlagUp == 0 {
				continue
			}
			for _, a := range mustAddrs(in) {
				ip := strings.Split(a, "/")[0]
				if strings.Count(ip, ".") != 3 {
					continue
				}
				url := fmt.Sprintf("http://%s:8089/api/discover", ip)
				code, body := httpCheck(url, 2*time.Second)
				fmt.Fprintf(&b, "%s  → HTTP %d  body=%s\n", url, code, body)
			}
		}
	}

	b.WriteString("\n=== 邻居表 ===\n")
	b.WriteString(runBrief("ip", "neigh"))

	// 服务端：反向探测 ZeroTier 网段上的客户端（8086），确认隧道是否双向可通
	if s.cfg.GetMode() == config.ModeServer {
		if vip := s.zt.GetVirtualIP(); vip != "" {
			b.WriteString("\n=== 反向探测客户端 (zt 网段:8086) ===\n")
			b.WriteString(reverseProbe(vip))
		}
	}

	// 客户端：直接测试到服务端的连通性，绕开扫描环节
	if s.cfg.GetMode() == config.ModeClient && s.cfg.Client.ServerAddr != "" {
		b.WriteString("\n=== 服务端连通性 ===\n")
		host, port, err := net.SplitHostPort(s.cfg.Client.ServerAddr)
		if err == nil {
			for _, p := range []string{port, "8081", "8089"} {
				addr := net.JoinHostPort(host, p)
				c, derr := net.DialTimeout("tcp", addr, 2*time.Second)
				if derr != nil {
					fmt.Fprintf(&b, "%s  失败：%v\n", addr, derr)
					continue
				}
				_ = c.Close()
				fmt.Fprintf(&b, "%s  可连接\n", addr)
			}
		} else {
			fmt.Fprintf(&b, "地址格式异常：%s\n", s.cfg.Client.ServerAddr)
		}
	}

	b.WriteString("\n=== 路由 ===\n")
	b.WriteString(runBrief("ip", "route"))

	writeJSON(w, http.StatusOK, map[string]string{"text": b.String()})
}

// dataPlaneVerdict 根据 ZeroTier 状态给出“两端能否真正通信”的结论。
// 这是排查“扫不到服务端”时最关键的信息：如果数据面就不通，扫描再多也没用。
func dataPlaneVerdict(st zerotier.Status) string {
	if !st.Running {
		return "ZeroTier 进程未运行"
	}
	if !st.NetworkConfigured {
		return "未加入网络或未拿到虚拟 IP"
	}
	if st.BroadcastEnabled != nil && !*st.BroadcastEnabled {
		return "网络禁用了广播(broadcastEnabled=false)，ZeroTier 不会转发 ARP，同网络成员永远互相不可达 —— 请在 ZeroTier Central 打开 Allow Broadcast / 或改用 Managed Route"
	}
	if st.PortError != 0 {
		return fmt.Sprintf("UDP 端口绑定异常 portError=%d（检查 9993 是否被占用/防火墙拦截）", st.PortError)
	}
	if st.ZTMTU > 1500 {
		return fmt.Sprintf("tun MTU=%d 超过物理链路 1500：ZeroTier 的 UDP 封装包需分片，常被 NAT 静默丢弃，导致成员间建立不了路径（程序会自动下调到 1400，重启后复查 active_leaf_peers）", st.ZTMTU)
	}
	if st.ActiveLeafPeers == 0 {
		return "已入网但没有任何成员建立可用路径（直连和中继都失败）——典型的 NAT 穿透失败：需任一端可公网直连、开启端口映射，或增加一个有公网 IP 的中继节点"
	}
	return "有成员建立了活跃路径；若仍扫不到服务端，请对照两端诊断里的 member=，确认对端已出现在本端 peers 列表且为 OK"
}

func runBrief(name string, args ...string) string {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("（%s 执行失败：%v）\n", name, err)
	}
	s := string(out)
	const max = 4000
	if len(s) > max {
		s = s[:max] + "\n（已截断）\n"
	}
	return s
}

func filterLines(s string, keys ...string) string {
	var b strings.Builder
	for _, ln := range strings.Split(s, "\n") {
		for _, k := range keys {
			if strings.Contains(ln, k) {
				b.WriteString(ln + "\n")
				break
			}
		}
	}
	if b.Len() == 0 {
		return "（无匹配监听）\n"
	}
	return b.String()
}

// httpCheck 发起一次 HTTP GET，返回状态码和响应体前 120 字符
func httpCheck(url string, timeout time.Duration) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Sprintf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	s := string(body)
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return resp.StatusCode, s
}

func mustAddrs(in net.Interface) []string {
	addrs, _ := in.Addrs()
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return out
}

// reverseProbe 从服务端向 zt 同网段的 8086（客户端本地代理端口）发起连接，
// 用于判断 ZeroTier 隧道是否双向可通、客户端进程是否在运行。
func reverseProbe(vip string) string {
	ip := net.ParseIP(vip).To4()
	if ip == nil {
		return "虚拟 IP 无法解析\n"
	}
	base := fmt.Sprintf("%d.%d.%d.", ip[0], ip[1], ip[2])

	var (
		mu      sync.Mutex
		alive   []string
		wg      sync.WaitGroup
		sem     = make(chan struct{}, 100)
		self    = vip
		timeout = 800 * time.Millisecond
	)

	for i := 1; i <= 254; i++ {
		host := base + strconv.Itoa(i)
		if host == self {
			continue
		}
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c, err := net.DialTimeout("tcp", net.JoinHostPort(h, "8086"), timeout)
			if err != nil {
				return
			}
			_ = c.Close()
			mu.Lock()
			alive = append(alive, h)
			mu.Unlock()
		}(host)
	}
	wg.Wait()

	if len(alive) == 0 {
		return fmt.Sprintf("网段 %s0/24 的 8086 端口无任何主机响应（隧道单向或客户端未运行）\n", base)
	}
	sort.Strings(alive)
	return fmt.Sprintf("以下地址 8086 可连接：%s\n", strings.Join(alive, ", "))
}
