package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/iptv-zt-bridge/internal/config"
	"github.com/iptv-zt-bridge/internal/firewall"
	"github.com/iptv-zt-bridge/internal/health"
	"github.com/iptv-zt-bridge/internal/m3u"
	"github.com/iptv-zt-bridge/internal/proxy"
	"github.com/iptv-zt-bridge/internal/rtp2httpd"
	"github.com/iptv-zt-bridge/internal/web"
	"github.com/iptv-zt-bridge/internal/zerotier"
)

var (
	Version   = "0.1.0"
	GitCommit = "unknown"
	BuildTime = "unknown"
)

func main() {
	var (
		configPath string
		webRoot    string
		dataDir    string
		modeFlag   string
		version    bool
		showArch   bool
	)

	flag.StringVar(&configPath, "config", "", "配置文件路径")
	flag.StringVar(&webRoot, "web-root", "", "Web 静态文件目录")
	flag.StringVar(&dataDir, "data-dir", "", "数据目录")
	flag.StringVar(&modeFlag, "mode", "", "强制运行模式: server | client（由 SPK 套件传入）")
	flag.BoolVar(&version, "version", false, "显示版本")
	flag.BoolVar(&showArch, "arch", false, "显示当前架构")
	flag.Parse()

	if version {
		fmt.Printf("IPTV-ZT-Bridge %s (commit: %s, build: %s, arch: %s)\n",
			Version, GitCommit, BuildTime, zerotier.DetectArch())
		return
	}
	if showArch {
		fmt.Println(zerotier.DetectArch())
		return
	}

	exePath, _ := os.Executable()
	exeDir := filepath.Dir(exePath)
	pkgRoot, packaged := resolvePackageRoot(exeDir)

	cfgPath := configPath
	if cfgPath == "" {
		if packaged {
			cfgPath = filepath.Join(pkgRoot, "etc", "config.yaml")
		} else {
			cwd, _ := os.Getwd()
			cfgPath = filepath.Join(cwd, "config.yaml")
		}
	}

	dr := dataDir
	if dr == "" {
		if packaged {
			dr = filepath.Join(pkgRoot, "var")
		} else {
			cwd, _ := os.Getwd()
			dr = filepath.Join(cwd, "data")
		}
	}

	wr := webRoot
	if wr == "" {
		candidates := []string{
			filepath.Join(filepath.Dir(exeDir), "web"),
			filepath.Join(exeDir, "web"),
		}
		if cwd, err := os.Getwd(); err == nil {
			candidates = append(candidates, filepath.Join(cwd, "web", "static"))
		}
		for _, c := range candidates {
			if st, err := os.Stat(c); err == nil && st.IsDir() {
				wr = c
				break
			}
		}
	}

	fmt.Printf("[iptv-zt-bridge] v%s arch=%s\n", Version, zerotier.DetectArch())
	fmt.Printf("[init] config=%s data=%s web=%s\n", cfgPath, dr, wr)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[init] load config failed: %v\n", err)
		os.Exit(1)
	}

	cfg.Runtime.ConfigFile = cfgPath
	cfg.Runtime.DataDir = dr
	cfg.Runtime.LogDir = filepath.Join(dr, "log")
	cfg.Runtime.WebRoot = wr

	// 套件模式优先，允许 -mode 覆盖配置
	if modeFlag != "" {
		m := config.Mode(strings.ToLower(modeFlag))
		if m != config.ModeServer && m != config.ModeClient {
			fmt.Fprintf(os.Stderr, "[init] invalid -mode %q (server|client)\n", modeFlag)
			os.Exit(1)
		}
		cfg.SetMode(m)
	}

	// 客户端：端口固定 8086；未手动指定服务端地址时通过 ZeroTier 自动发现
	if cfg.GetMode() == config.ModeClient {
		cfg.Client.ListenAddr = "0.0.0.0:8086"
	}

	// 解析内置二进制路径（不硬编码包名）
	if cfg.ZeroTier.BinaryPath == "" {
		cfg.ZeroTier.BinaryPath = filepath.Join(exeDir, "zerotier-one")
	}
	if cfg.ZeroTier.DataDir == "" {
		cfg.ZeroTier.DataDir = filepath.Join(dr, "zerotier")
	}
	if cfg.RTP2HTTPD.BinaryPath == "" {
		cfg.RTP2HTTPD.BinaryPath = filepath.Join(exeDir, "rtp2httpd")
	}
	if cfg.RTP2HTTPD.ConfigPath == "" {
		cfg.RTP2HTTPD.ConfigPath = filepath.Join(dr, "rtp2httpd", "rtp2httpd.conf")
	}

	for _, d := range []string{cfg.Runtime.DataDir, cfg.Runtime.LogDir, cfg.ZeroTier.DataDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "[init] mkdir %s: %v\n", d, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ztMgr := zerotier.NewManager(cfg)
	streamProxy := proxy.NewProxy(time.Duration(cfg.Client.UpstreamTimeout) * time.Second)
	m3uParser := m3u.NewParser()

	var rtpMgr *rtp2httpd.Manager
	var playlist *m3u.Playlist

	if cfg.GetMode() == config.ModeServer {
		rtpMgr = rtp2httpd.New(cfg)

		if strings.TrimSpace(cfg.RTP2HTTPD.SourceM3U) != "" {
			fmt.Println("[rtp2httpd] starting bundled rtp2httpd...")
			if err := rtpMgr.Start(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "[rtp2httpd] start failed: %v\n", err)
			} else {
				if err := rtpMgr.WaitReady(ctx, 25*time.Second); err != nil {
					fmt.Fprintf(os.Stderr, "[rtp2httpd] not ready: %v\n", err)
				} else {
					fmt.Println("[rtp2httpd] ready")
				}
			}
		} else {
			fmt.Println("[rtp2httpd] 未上传频道源 M3U，等待 WebUI 上传后启动")
		}

		// 选择桥接层使用的源：
		// - 含 rtp:// / udp:// 组播 → 走内置 rtp2httpd 转换后的列表
		// - 纯 http(s)/rtsp 单播（如 HLS）→ 直接使用上传的源，由桥接层自行代理
		cfg.Server.SourceM3U = resolveBridgeSource(ctx, cfg, m3uParser, rtpMgr)

		fmt.Printf("[mode] Server - source=%s\n", cfg.Server.SourceM3U)
		playlist = &m3u.Playlist{}
		if pl, err := loadServerPlaylist(ctx, cfg, m3uParser); err == nil {
			playlist = pl
			for _, e := range playlist.Entries {
				e.Alive = true
			}
			playlist.ApplyToken(cfg.Token, cfg.Server.StreamPath)
			fmt.Printf("[server] loaded %d entries\n", playlist.Count())
		} else {
			fmt.Fprintf(os.Stderr, "[server] initial load failed: %v (will retry)\n", err)
		}
	} else {
		fmt.Println("[mode] Client - initializing...")
		playlist = &m3u.Playlist{}
		// 客户端已不再提供“手动指定服务端”，配置文件里存的地址同样来自自动发现，
		// 因此启动即视为可自动刷新，拉取失败时允许清空并重新发现。
		autoDiscovered.Store(true)
	}

	webSrv := web.NewServer(cfg, ztMgr, streamProxy, rtpMgr)
	webSrv.SetVersion(Version)
	webSrv.SetPlaylist(playlist)

	if cfg.GetMode() == config.ModeServer {
		mon := health.NewMonitor(ztMgr,
			time.Duration(cfg.Server.HealthCheckInterval)*time.Second,
			time.Duration(cfg.Server.HealthCheckTimeout)*time.Second)
		mon.SetPlaylist(playlist)
		mon.OnPlaylistUpdate(func(pl *m3u.Playlist) { webSrv.SetPlaylist(pl) })
		go mon.Run(ctx)

		go serverRefreshLoop(ctx, cfg, m3uParser, playlist, webSrv, rtpMgr)
		webSrv.OnRefresh(func() { go reloadServerPlaylist(ctx, cfg, m3uParser, playlist, webSrv, rtpMgr) })

		// 周期性反向探测 ZeroTier 网段上的客户端（8086），结果写入日志，
		// 便于在无法使用命令行的系统上判断隧道是否双向可通。
		go func() {
			time.Sleep(20 * time.Second)
			for {
				if vip := ztMgr.GetVirtualIP(); vip != "" {
					probeLanClients(vip)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(60 * time.Second):
				}
			}
		}()
	} else {
		go clientRefreshLoop(ctx, cfg, m3uParser, playlist, webSrv, ztMgr)
		webSrv.OnRefresh(func() { go refreshClientOnce(ctx, cfg, m3uParser, playlist, webSrv, ztMgr) })
	}

	if cfg.ZeroTier.NetworkID != "" {
		fmt.Println("[init] auto-starting ZeroTier...")
		go func() {
			time.Sleep(time.Second)
			if err := ztMgr.Start(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "[zt] auto-start failed: %v\n", err)
			}
		}()
	}

	go func() {
		if err := webSrv.Start(ctx); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[web] server error: %v\n", err)
		}
	}()

	// 对外 m3u / 流代理服务（独立端口）
	publicAddr := cfg.Server.ListenAddr
	if cfg.GetMode() == config.ModeClient {
		publicAddr = cfg.Client.ListenAddr
	}
	publicSrv := &http.Server{Addr: publicAddr, Handler: webSrv.PublicHandler()}
	go func() {
		fmt.Printf("[public] m3u/stream listening on %s\n", publicAddr)
		if err := publicSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[public] server error: %v\n", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, sc := context.WithTimeout(context.Background(), 3*time.Second)
		defer sc()
		_ = publicSrv.Shutdown(shutdownCtx)
	}()

	setupFirewall(cfg)

	fmt.Println("[init] ready. waiting for signals...")

	sig := <-sigCh
	fmt.Printf("\n[signal] received %v, shutting down...\n", sig)

	cancel()
	time.Sleep(300 * time.Millisecond)
	if rtpMgr != nil {
		rtpMgr.Stop()
	}
	if err := ztMgr.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "[zt] stop: %v\n", err)
	}
	fmt.Println("[bye] exited cleanly.")
}

// resolvePackageRoot 判断是否运行在 DSM 包目录中
func resolvePackageRoot(exeDir string) (string, bool) {
	if name := os.Getenv("SYNOPKG_PKGNAME"); name != "" {
		return filepath.Join("/var/packages", name), true
	}
	// target/bin -> target -> package
	targetDir := filepath.Dir(exeDir)
	if filepath.Base(targetDir) == "target" {
		root := filepath.Dir(targetDir)
		if strings.Contains(root, "/var/packages/") {
			return root, true
		}
	}
	return "", false
}

func loadServerPlaylist(ctx context.Context, cfg *config.Config, p *m3u.Parser) (*m3u.Playlist, error) {
	return loadPlaylistFrom(ctx, cfg.Server.SourceM3U, p)
}

func loadPlaylistFrom(ctx context.Context, src string, p *m3u.Parser) (*m3u.Playlist, error) {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		return p.ParseURL(ctx, src)
	}
	if strings.HasPrefix(src, "file://") {
		return p.ParseFile(strings.TrimPrefix(src, "file://"))
	}
	return p.ParseFile(src)
}

// resolveBridgeSource 决定桥接层读取哪个列表
func resolveBridgeSource(ctx context.Context, cfg *config.Config, p *m3u.Parser,
	rtpMgr *rtp2httpd.Manager) string {

	src := strings.TrimSpace(cfg.RTP2HTTPD.SourceM3U)
	if src == "" {
		return rtpMgr.PlaylistURL()
	}
	if pl, err := loadPlaylistFrom(ctx, src, p); err == nil {
		if !hasMulticast(pl) {
			// 纯单播源，rtp2httpd 无用武之地，直接代理原始地址
			return src
		}
	}
	return rtpMgr.PlaylistURL()
}

func hasMulticast(pl *m3u.Playlist) bool {
	for _, e := range pl.EntriesCopy() {
		u := strings.ToLower(e.OriginalURL)
		if strings.HasPrefix(u, "rtp://") || strings.HasPrefix(u, "udp://") {
			return true
		}
	}
	return false
}

// serverRefreshLoop 周期性刷新服务端列表（空列表时快速重试）
func serverRefreshLoop(ctx context.Context, cfg *config.Config, p *m3u.Parser,
	pl *m3u.Playlist, ws *web.Server, rtpMgr *rtp2httpd.Manager) {

	// 上传新源后需要重新决定使用原始源还是 rtp2httpd 列表（见 reloadServerPlaylist）

	// 放行防火墙（幂等，仅首次执行）
	fwOnce.Do(func() {
		firewall.Open(
			[]int{parsePort(cfg.Server.ListenAddr), parsePort(cfg.Web.ListenAddr)},
			[]int{9993},
		)
	})

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	var lastLoad time.Time
	if pl.Count() > 0 {
		lastLoad = time.Now()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if pl.Count() > 0 && time.Since(lastLoad) < 10*time.Minute {
				continue
			}
			if reloadServerPlaylist(ctx, cfg, p, pl, ws, rtpMgr) {
				lastLoad = time.Now()
			}
		}
	}
}

// reloadServerPlaylist 重新加载并应用 Token，返回是否成功
func reloadServerPlaylist(ctx context.Context, cfg *config.Config, p *m3u.Parser,
	pl *m3u.Playlist, ws *web.Server, rtpMgr *rtp2httpd.Manager) bool {

	if rtpMgr != nil && strings.TrimSpace(cfg.RTP2HTTPD.SourceM3U) != "" {
		cfg.Server.SourceM3U = resolveBridgeSource(ctx, cfg, p, rtpMgr)
	}

	newPl, err := loadServerPlaylist(ctx, cfg, p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[refresh] load failed: %v\n", err)
		return false
	}
	for _, e := range newPl.Entries {
		e.Alive = true
	}
	newPl.ApplyToken(cfg.Token, cfg.Server.StreamPath)
	pl.Replace(newPl.Entries)
	ws.SetPlaylist(pl)
	fmt.Printf("[refresh] playlist updated, %d entries\n", newPl.Count())
	return true
}

// clientRefreshLoop 客户端周期性拉取服务端列表
func clientRefreshLoop(ctx context.Context, cfg *config.Config, p *m3u.Parser,
	pl *m3u.Playlist, ws *web.Server, zt *zerotier.Manager) {

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	if refreshClientOnce(ctx, cfg, p, pl, ws, zt) {
		ticker.Reset(2 * time.Minute)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if refreshClientOnce(ctx, cfg, p, pl, ws, zt) {
				ticker.Reset(2 * time.Minute)
			} else {
				// 失败时回退到较快的重试节奏，避免长时间停留在错误状态
				ticker.Reset(60 * time.Second)
			}
		}
	}
}

func refreshClientOnce(ctx context.Context, cfg *config.Config, p *m3u.Parser,
	pl *m3u.Playlist, ws *web.Server, zt *zerotier.Manager) bool {

	st := zt.Status()
	if !st.NetworkOnline {
		// 隧道未就绪：把真实原因写进 WebUI，不要一直显示“自动发现中”
		msg := fmt.Sprintf("等待 ZeroTier 入网（member=%s", dash(st.MemberAddress))
		if len(st.VirtualAddresses) > 0 {
			msg += "，IP=" + strings.Join(st.VirtualAddresses, ",")
		}
		msg += "）"
		if reason := ztDataPlaneHint(st); reason != "" {
			msg += "；" + reason
		}
		ws.SetDiscoverState(msg)
		return false
	}

	// 放行防火墙（幂等，仅首次执行）
	fwOnce.Do(func() {
		firewall.Open(
			[]int{parsePort(cfg.Client.ListenAddr), parsePort(cfg.Web.ListenAddr), 8086},
			[]int{9993},
		)
	})

	// 服务端地址与 Token 未知时，在 ZeroTier 网段内自动发现
	if cfg.Client.ServerAddr == "" || cfg.Token == "" {
		ws.SetDiscoverState(fmt.Sprintf("正在扫描 %s:8081/8089 …", scanDesc(zt)))
		start := time.Now()
		addr, token, m3uPath, stats, ok := discoverServer(ctx, zt)
		if !ok {
			msg := fmt.Sprintf("未发现服务端（扫描 %s:8081/8089，耗时 %.1fs，%s）",
				scanDesc(zt), time.Since(start).Seconds(), stats)
			if reason := ztDataPlaneHint(zt.Status()); reason != "" {
				msg += "；" + reason
			}
			ws.SetDiscoverState(msg)
			fmt.Fprintln(os.Stderr, "[client] "+msg)
			return false
		}
		cfg.Client.ServerAddr = addr
		autoDiscovered.Store(true)
		if m3uPath != "" {
			cfg.Client.ServerM3UPath = m3uPath
		}
		ws.SetToken(token)
		if cfg.Runtime.ConfigFile != "" {
			_ = cfg.Save(cfg.Runtime.ConfigFile)
		}
		fmt.Printf("[client] 已发现服务端 %s，Token 已同步\n", addr)
	}

	serverURL := fmt.Sprintf("http://%s%s?token=%s",
		cfg.Client.ServerAddr, cfg.Client.ServerM3UPath, cfg.Token)

	newPl, err := p.ParseURL(ctx, serverURL)
	if err != nil {
		ws.SetDiscoverState(fmt.Sprintf("已发现 %s，但拉取播放列表失败：%v（下一轮重试）",
			cfg.Client.ServerAddr, err))
		fmt.Fprintf(os.Stderr, "[client] fetch m3u failed: %v\n", err)
		// 地址/Token 可能已过期（服务端重启、重新生成 Token、IP 变化）：
		// 清掉以便下一轮重新发现，而不是永远卡在旧地址上。
		if autoDiscovered.Load() {
			cfg.Client.ServerAddr = ""
			cfg.Token = ""
			ws.SetToken("")
		}
		return false
	}

	newPl.ApplyLocalProxy(cfg.Client.StreamPath, cfg.Token)

	pl.Replace(newPl.Entries)
	ws.SetPlaylist(pl)
	ws.SetDiscoverState(fmt.Sprintf("已发现 %s，%s 同步 %d 个频道",
		cfg.Client.ServerAddr, time.Now().Format("15:04:05"), newPl.Count()))
	fmt.Printf("[client] playlist updated, %d entries\n", newPl.Count())
	return true
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ztNetSegment 返回 ZeroTier 虚拟 IP 所在 /24 网段，用于展示与扫描
func ztNetSegment(zt *zerotier.Manager) string {
	ip := net.ParseIP(zt.GetVirtualIP()).To4()
	if ip == nil {
		return "未知网段"
	}
	return fmt.Sprintf("%d.%d.%d.0/24", ip[0], ip[1], ip[2])
}

// scanDesc 描述当前会扫描的网段（ZeroTier + 本机局域网）
func scanDesc(zt *zerotier.Manager) string {
	bases := localScanBases(zt.GetVirtualIP())
	parts := make([]string, 0, len(bases))
	for _, b := range bases {
		parts = append(parts, b+"0/24")
	}
	if len(parts) == 0 {
		return "未知网段"
	}
	return strings.Join(parts, " + ")
}

// discoverServer 在 ZeroTier 同网段内扫描服务端发现接口
func discoverServer(ctx context.Context, zt *zerotier.Manager) (addr, token, m3uPath, stats string, ok bool) {
	vip := zt.GetVirtualIP()
	ip := net.ParseIP(vip).To4()
	if ip == nil {
		return "", "", "", "无 ZeroTier 虚拟 IP", false
	}
	atomic.StoreInt64(&refusedCount, 0)
	atomic.StoreInt64(&timeoutCount, 0)

	type found struct {
		ip    string
		token string
		m3u   string
	}

	// 同时探测 8081（专用对外端口）与 8089（WebUI/service_port 端口）。
	// 部分系统（飞牛 fnOS）只放行服务声明的端口，8081 会被静默丢弃。
	ports := []string{"8081", "8089"}

	// 候选网段：ZeroTier 虚拟网段 + 本机各物理网卡的 /24。
	// 服务端与客户端在同一局域网时（测试或同处一网），即使 ZeroTier 路径不通
	// 也能通过局域网直连发现，避免“隧道故障导致整个产品不可用”。
	bases := localScanBases(vip)

	// 两轮扫描：
	// - 第一轮用较短超时先“预热”——首个 SYN 会触发 ZeroTier 与目标成员协商
	//   打洞/中继路径，这个过程常超过 1.5s，所以第一轮即便失败也不算数；
	// - 第二轮用更长超时补扫，此时路径已被预热，正常情况下能命中。
	for _, timeout := range []time.Duration{1500 * time.Millisecond, 3500 * time.Millisecond} {
		if ctx.Err() != nil {
			break
		}
		ch := make(chan found, 64)
		sem := make(chan struct{}, 128)
		var wg sync.WaitGroup
		client := &http.Client{Timeout: timeout}

		for _, base := range bases {
			for i := 1; i <= 254; i++ {
				host := base + strconv.Itoa(i)
				if host == vip {
					continue
				}
				wg.Add(1)
				go func(h string) {
					defer wg.Done()
					select {
					case sem <- struct{}{}:
					case <-ctx.Done():
						return
					}
					defer func() { <-sem }()

					for _, port := range ports {
						req, err := http.NewRequestWithContext(ctx, http.MethodGet,
							fmt.Sprintf("http://%s:%s/api/discover", h, port), nil)
						if err != nil {
							continue
						}
						resp, err := client.Do(req)
						if err != nil {
							var ne net.Error
							if errors.As(err, &ne) && ne.Timeout() {
								atomic.AddInt64(&timeoutCount, 1)
							} else {
								atomic.AddInt64(&refusedCount, 1)
							}
							continue
						}
						var d struct {
							App   string `json:"app"`
							Role  string `json:"role"`
							M3U   string `json:"m3u"`
							Token string `json:"token"`
						}
						derr := json.NewDecoder(resp.Body).Decode(&d)
						resp.Body.Close()
						if resp.StatusCode != http.StatusOK || derr != nil {
							continue
						}
						if d.App != "IPTV-ZT-Bridge" || d.Role != "server" || d.Token == "" {
							continue
						}
						select {
						case ch <- found{h + ":" + port, d.Token, d.M3U}:
						default:
						}
						return
					}
				}(host)
			}
		}

		go func() {
			wg.Wait()
			close(ch)
		}()

		for r := range ch {
			return r.ip, r.token, r.m3u, "", true
		}
	}
	refused := atomic.LoadInt64(&refusedCount)
	timeouts := atomic.LoadInt64(&timeoutCount)
	stats = fmt.Sprintf("超时 %d，连接被拒 %d（两轮扫描均未命中）", timeouts, refused)
	return "", "", "", stats, false
}

// ztDataPlaneHint 返回导致“扫不到服务端”的 ZeroTier 层原因（空串表示没有明显障碍）。
func ztDataPlaneHint(st zerotier.Status) string {
	if !st.TunMode {
		return "ZeroTier 未创建虚拟网卡，无法访问对端 IP"
	}
	if st.ZTMTU > 1500 {
		return fmt.Sprintf("ZeroTier tun MTU=%d 过大，UDP 封装包分片易被 NAT 丢弃，导致成员间建不了路径（程序会下调到 1400）", st.ZTMTU)
	}
	if !st.TunnelRouteOK {
		return "ZeroTier 路由缺失（隧道内地址不可达）"
	}
	if st.BroadcastEnabled != nil && !*st.BroadcastEnabled {
		return "ZeroTier 网络禁用了广播(broadcastEnabled=false)，ARP 不通，同网络成员永远互不可达"
	}
	if st.ActiveLeafPeers == 0 {
		return "ZeroTier 已入网但没有任何成员建立活跃路径（NAT 穿透失败，需端口映射或公网中继）"
	}
	return ""
}

// localScanBases 返回待扫描的 /24 前缀列表：ZeroTier 网段优先，其次是本机物理网卡所在网段。
func localScanBases(vip string) []string {
	seen := map[string]bool{}
	var bases []string

	add := func(ip net.IP) {
		v4 := ip.To4()
		if v4 == nil {
			return
		}
		base := fmt.Sprintf("%d.%d.%d.", v4[0], v4[1], v4[2])
		if seen[base] {
			return
		}
		seen[base] = true
		bases = append(bases, base)
	}

	if v := net.ParseIP(vip).To4(); v != nil {
		add(v)
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return bases
	}
	for _, in := range ifaces {
		if in.Flags&net.FlagUp == 0 || in.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := in.Name
		if strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") ||
			strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "zt") {
			continue
		}
		addrs, err := in.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok {
				if ipnet.IP.IsPrivate() && ipnet.IP.To4() != nil {
					add(ipnet.IP)
				}
			}
		}
	}
	return bases
}

var (
	refusedCount int64
	timeoutCount int64
)

// probeLanClients 从服务端反向连接 zt 同网段的 8086（客户端本地代理端口），
// 用于判断隧道是否双向可通。结果写入日志（同一结果不重复打印）。
func probeLanClients(vip string) {
	ip := net.ParseIP(vip).To4()
	if ip == nil {
		return
	}
	base := fmt.Sprintf("%d.%d.%d.", ip[0], ip[1], ip[2])

	var (
		mu    sync.Mutex
		alive []string
		wg    sync.WaitGroup
		sem   = make(chan struct{}, 100)
	)
	for i := 1; i <= 254; i++ {
		host := base + strconv.Itoa(i)
		if host == vip {
			continue
		}
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c, err := net.DialTimeout("tcp", net.JoinHostPort(h, "8086"), 700*time.Millisecond)
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

	summary := "无响应（隧道单向或客户端未运行）"
	if len(alive) > 0 {
		summary = strings.Join(alive, ",") + " 的 8086 可连接"
	}

	lastProbeMu.Lock()
	changed := summary != lastProbeSummary
	lastProbeSummary = summary
	lastProbeMu.Unlock()
	if changed {
		fmt.Printf("[probe] 反向探测 %s0/24:8086 → %s\n", base, summary)
	}
}

var (
	lastProbeMu      sync.Mutex
	lastProbeSummary string
)

// fwOnce 保证防火墙放行只执行一次
var fwOnce sync.Once

// autoDiscovered 标记服务端地址是否来自自动发现
var autoDiscovered atomic.Bool

func parsePort(addr string) int {
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return 0
	}
	p, err := strconv.Atoi(addr[idx+1:])
	if err != nil {
		return 0
	}
	return p
}

func setupFirewall(cfg *config.Config) {
	var tcpPorts []int
	if cfg.Web.ListenAddr != "" {
		tcpPorts = append(tcpPorts, parsePort(cfg.Web.ListenAddr))
	}
	if cfg.GetMode() == config.ModeServer {
		tcpPorts = append(tcpPorts, parsePort(cfg.Server.ListenAddr))
	} else {
		tcpPorts = append(tcpPorts, parsePort(cfg.Client.ListenAddr))
	}

	// 立即放行（幂等），确保 WebUI / m3u / ZeroTier UDP 都能被其他主机访问
	firewall.Open(tcpPorts, []int{9993})

	fmt.Printf("[firewall] tcp=%v udp=[9993]\n", tcpPorts)
	fmt.Println("[firewall] 如被 DSM/fnOS 防火墙拦截，请在系统防火墙界面放行上述端口")
}
