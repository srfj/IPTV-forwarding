package rtp2httpd

import (
	"context"
	"fmt"
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

// Status rtp2httpd 运行状态
type Status struct {
	Enabled     bool      `json:"enabled"`
	Running     bool      `json:"running"`
	PID         int       `json:"pid"`
	ListenAddr  string    `json:"listen_addr"`
	SourceM3U   string    `json:"source_m3u"`
	PlaylistURL string    `json:"playlist_url"`
	ConfigPath  string    `json:"config_path"`
	StartedAt   time.Time `json:"started_at"`
	LastError   string    `json:"last_error,omitempty"`
}

// Manager 内置 rtp2httpd 管理器
type Manager struct {
	cfg *config.Config

	mu       sync.RWMutex
	proc     *os.Process
	cmd      *exec.Cmd
	running  bool
	stopping bool
	status   Status

	// 进程生命周期上下文（不受单次 HTTP 请求取消影响）
	baseCtx context.Context
}

// New 创建管理器
func New(cfg *config.Config) *Manager {
	return &Manager{
		cfg:     cfg,
		baseCtx: context.Background(),
		status: Status{
			Enabled:    true,
			ListenAddr: cfg.RTP2HTTPD.ListenAddr,
			SourceM3U:  cfg.RTP2HTTPD.SourceM3U,
			ConfigPath: cfg.RTP2HTTPD.ConfigPath,
		},
	}
}

// PlaylistURL rtp2httpd 输出的 m3u 地址（本机访问）
func (m *Manager) PlaylistURL() string {
	return playlistURL(m.cfg.RTP2HTTPD.ListenAddr)
}

func playlistURL(listenAddr string) string {
	host, port := splitAddr(listenAddr)
	if host == "" || host == "0.0.0.0" || host == "::" || host == "*" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%s/playlist.m3u", host, port)
}

func splitAddr(addr string) (string, string) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		idx := strings.LastIndex(addr, ":")
		if idx < 0 {
			return addr, "8080"
		}
		return addr[:idx], addr[idx+1:]
	}
	return host, port
}

// Start 启动内置 rtp2httpd
func (m *Manager) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil
	}
	m.stopping = false
	m.mu.Unlock()

	bin := m.cfg.RTP2HTTPD.BinaryPath
	if bin == "" {
		return fmt.Errorf("rtp2httpd 二进制路径未配置")
	}
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("rtp2httpd 二进制不存在: %s", bin)
	}

	if strings.TrimSpace(m.cfg.RTP2HTTPD.SourceM3U) == "" {
		return fmt.Errorf("未配置 IPTV 频道源 M3U（WebUI 中填写运营商频道列表地址或本地文件）")
	}

	confPath := m.cfg.RTP2HTTPD.ConfigPath
	if confPath == "" {
		return fmt.Errorf("rtp2httpd 配置路径未配置")
	}
	if err := os.MkdirAll(filepath.Dir(confPath), 0755); err != nil {
		return fmt.Errorf("创建 rtp2httpd 配置目录失败: %w", err)
	}

	conf, err := m.generateConfig()
	if err != nil {
		return err
	}
	if err := os.WriteFile(confPath, []byte(conf), 0644); err != nil {
		return fmt.Errorf("写入 rtp2httpd 配置失败: %w", err)
	}

	cmd := exec.CommandContext(m.baseCtx, bin, "--config", confPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		m.setStatus(func(s *Status) { s.LastError = err.Error() })
		return fmt.Errorf("启动 rtp2httpd 失败: %w", err)
	}

	m.mu.Lock()
	m.cmd = cmd
	m.proc = cmd.Process
	m.running = true
	m.status = Status{
		Enabled:     true,
		Running:     true,
		PID:         cmd.Process.Pid,
		ListenAddr:  m.cfg.RTP2HTTPD.ListenAddr,
		SourceM3U:   m.cfg.RTP2HTTPD.SourceM3U,
		PlaylistURL: playlistURL(m.cfg.RTP2HTTPD.ListenAddr),
		ConfigPath:  confPath,
		StartedAt:   time.Now(),
	}
	m.mu.Unlock()

	fmt.Printf("[rtp2httpd] started pid=%d listen=%s source=%s\n",
		cmd.Process.Pid, m.cfg.RTP2HTTPD.ListenAddr, m.cfg.RTP2HTTPD.SourceM3U)

	// 异步等待退出，非主动停止时自动重启
	go m.waitAndRestart(m.baseCtx, cmd)

	return nil
}

// Stop 停止 rtp2httpd
func (m *Manager) Stop() {
	m.mu.Lock()
	m.stopping = true
	proc := m.proc
	m.mu.Unlock()

	if proc != nil {
		_ = proc.Signal(os.Interrupt)
		waitExit(proc, 3*time.Second)
		_ = proc.Kill()
	}

	m.mu.Lock()
	m.proc = nil
	m.cmd = nil
	m.running = false
	m.status.Running = false
	m.status.PID = 0
	m.mu.Unlock()
}

func waitExit(p *os.Process, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if err := p.Signal(syscall.Signal(0)); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (m *Manager) waitAndRestart(ctx context.Context, cmd *exec.Cmd) {
	err := cmd.Wait()

	m.mu.Lock()
	m.running = false
	m.proc = nil
	m.status.Running = false
	m.status.PID = 0
	stopping := m.stopping
	if err != nil && !stopping {
		m.status.LastError = err.Error()
	}
	m.mu.Unlock()

	if stopping || ctx.Err() != nil {
		// 正常停止（重启、重新加载配置）不打印，避免误导
		return
	}

	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	if err == nil {
		fmt.Fprintf(os.Stderr, "[rtp2httpd] exited unexpectedly: code=%d\n", code)
	} else {
		fmt.Fprintf(os.Stderr, "[rtp2httpd] exited unexpectedly: %v code=%d\n", err, code)
	}

	select {
	case <-ctx.Done():
		return
	case <-time.After(3 * time.Second):
	}

	fmt.Println("[rtp2httpd] restarting...")
	if err := m.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "[rtp2httpd] restart failed: %v\n", err)
	}
}

// WaitReady 等待 rtp2httpd 的 playlist 可访问
func (m *Manager) WaitReady(ctx context.Context, timeout time.Duration) error {
	url := m.PlaylistURL()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}

	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("rtp2httpd 未就绪（%s）", url)
}

// Status 返回状态副本
func (m *Manager) Status() Status {
	m.mu.RLock()
	s := m.status
	m.mu.RUnlock()

	// 配置可能在运行期间被修改（如上传新频道源），这里同步最新值
	s.SourceM3U = m.cfg.RTP2HTTPD.SourceM3U
	s.ListenAddr = m.cfg.RTP2HTTPD.ListenAddr
	return s
}

func (m *Manager) setStatus(fn func(*Status)) {
	m.mu.Lock()
	fn(&m.status)
	m.mu.Unlock()
}

// generateConfig 生成 rtp2httpd 配置
func (m *Manager) generateConfig() (string, error) {
	src := strings.TrimSpace(m.cfg.RTP2HTTPD.SourceM3U)
	if !strings.Contains(src, "://") {
		// 本地路径转为 file://
		src = "file://" + src
	}

	host, port := splitAddr(m.cfg.RTP2HTTPD.ListenAddr)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "*"
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", fmt.Errorf("无效的 rtp2httpd 监听端口: %s", port)
	}

	var sb strings.Builder
	sb.WriteString("# Generated by IPTV-ZT-Bridge, do not edit manually.\n")
	sb.WriteString("[global]\n")
	sb.WriteString("verbosity = 3\n")
	sb.WriteString("maxclients = 32\n")
	sb.WriteString(fmt.Sprintf("external-m3u = %s\n", src))
	sb.WriteString("external-m3u-update-interval = 3600\n")
	if extra := strings.TrimSpace(m.cfg.RTP2HTTPD.ExtraConf); extra != "" {
		sb.WriteString(extra)
		sb.WriteString("\n")
	}
	sb.WriteString("\n[bind]\n")
	sb.WriteString(fmt.Sprintf("%s %s\n", host, port))

	return sb.String(), nil
}
