package health

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/iptv-zt-bridge/internal/m3u"
	"github.com/iptv-zt-bridge/internal/zerotier"
)

// Monitor 健康检测监控
type Monitor struct {
	zt       *zerotier.Manager
	interval time.Duration
	timeout  time.Duration

	currentPlaylist *m3u.Playlist
	mu              sync.RWMutex

	onPlaylistUpdate func(*m3u.Playlist)
}

// NewMonitor 创建监控
func NewMonitor(zt *zerotier.Manager, interval, timeout time.Duration) *Monitor {
	return &Monitor{
		zt:       zt,
		interval: interval,
		timeout:  timeout,
	}
}

// SetPlaylist 设置当前播放列表
func (m *Monitor) SetPlaylist(pl *m3u.Playlist) {
	m.mu.Lock()
	m.currentPlaylist = pl
	m.mu.Unlock()
}

// Playlist 获取当前列表
func (m *Monitor) Playlist() *m3u.Playlist {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.currentPlaylist
}

// OnPlaylistUpdate 设置更新回调
func (m *Monitor) OnPlaylistUpdate(fn func(*m3u.Playlist)) {
	m.onPlaylistUpdate = fn
}

// Run 后台持续检测
func (m *Monitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	// 启动后先等待：rtp2httpd 和上游源刚起来时首包很慢，
	// 立刻检测会把大量可用频道误判为失效。
	select {
	case <-ctx.Done():
		return
	case <-time.After(60 * time.Second):
	}

	m.check(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.check(ctx)
		}
	}
}

func (m *Monitor) check(ctx context.Context) {
	m.mu.RLock()
	pl := m.currentPlaylist
	m.mu.RUnlock()

	if pl == nil || pl.Count() == 0 {
		return
	}

	// ZeroTier 未在线时跳过
	if m.zt != nil && !m.zt.Status().NetworkOnline {
		return
	}

	checker := m3u.NewHealthChecker(m.timeout)
	checker.CheckAll(ctx, pl)

	alive := pl.CountAlive()
	fmt.Printf("[health] 检测完成：%d/%d 可用\n", alive, pl.Count())

	if m.onPlaylistUpdate != nil {
		m.onPlaylistUpdate(pl)
	}
}

// TunnelHealth ZeroTier 隧道健康检测
type TunnelHealth struct {
	zt          *zerotier.Manager
	targetAddr  string
	targetPort  int
	interval    time.Duration
	lastCheck   time.Time
	lastOK      bool
	consecutive bool
	failCount   int
	mu          sync.RWMutex

	onDisconnect func()
	onReconnect func()
}

// NewTunnelHealth 创建隧道健康检测
func NewTunnelHealth(zt *zerotier.Manager, targetAddr string, targetPort, intervalSec int) *TunnelHealth {
	if intervalSec <= 0 {
		intervalSec = 60
	}
	return &TunnelHealth{
		zt:         zt,
		targetAddr: targetAddr,
		targetPort: targetPort,
		interval:   time.Duration(intervalSec) * time.Second,
	}
}

// SetCallbacks 设置断线/重连回调
func (t *TunnelHealth) SetCallbacks(onDisconnect, onReconnect func()) {
	t.onDisconnect = onDisconnect
	t.onReconnect = onReconnect
}

// Run 持续检测
func (t *TunnelHealth) Run(ctx context.Context) {
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()

	t.check(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.check(ctx)
		}
	}
}

func (t *TunnelHealth) check(ctx context.Context) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.lastCheck = time.Now()

	// 通过 ZeroTier 管理器的 tunnel test
	ok := false
	if t.zt != nil {
		ok, _ = t.zt.TunnelTest(t.targetAddr)
	}

	if ok {
		if !t.lastOK {
			// 从离线变在线
			t.consecutive = false
			t.failCount = 0
			if t.onReconnect != nil {
				go t.onReconnect()
			}
		}
		t.lastOK = true
	} else {
		t.failCount++
		if t.failCount >= 3 && t.lastOK {
			// 连续 3 次失败才判定断线
			t.lastOK = false
			if t.onDisconnect != nil {
				go t.onDisconnect()
			}
		}
	}
}

// Status 返回状态
func (t *TunnelHealth) Status() map[string]interface{} {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return map[string]interface{}{
		"last_check":   t.lastCheck,
		"online":       t.lastOK,
		"fail_count":   t.failCount,
		"target_addr":  t.targetAddr,
		"target_port":  t.targetPort,
	}
}
