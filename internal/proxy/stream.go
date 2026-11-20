package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/iptv-zt-bridge/internal/m3u"
)

// Proxy HTTP 代理转发器
type Proxy struct {
	upstreamTimeout time.Duration
	streamTimeout   time.Duration
	client          *http.Client
	mu              sync.RWMutex
	activeStreams   int
}

// NewProxy 创建代理
func NewProxy(upstreamTimeout time.Duration) *Proxy {
	if upstreamTimeout <= 0 {
		upstreamTimeout = 30 * time.Second
	}
	return &Proxy{
		upstreamTimeout: upstreamTimeout,
		streamTimeout:   6 * time.Hour, // 直播流可以持续很久
		client: &http.Client{
			// 不设 Client.Timeout：它覆盖整个响应体读取，会把直播流在 N 秒后切断。
			// 仅用 ResponseHeaderTimeout 限制"等到响应头"的时间。
			Transport: &http.Transport{
				DialContext:           dialContextWithTimeout(10 * time.Second),
				ResponseHeaderTimeout: upstreamTimeout,
				TLSHandshakeTimeout:   5 * time.Second,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				DisableCompression:    true,
			},
		},
	}
}

// dialContextWithTimeout 带超时的拨号
func dialContextWithTimeout(d time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		var dialer net.Dialer
		dialer.Timeout = d
		dialer.KeepAlive = 30 * time.Second
		return dialer.DialContext(ctx, network, addr)
	}
}

// HandleStream 处理流代理请求
// encoded: 编码后的上游 URL
func (p *Proxy) HandleStream(w http.ResponseWriter, r *http.Request, encoded string) {
	originalURL, err := m3u.DecodeEncodedURL(encoded)
	if err != nil {
		http.Error(w, "invalid stream url", http.StatusBadRequest)
		return
	}

	p.mu.Lock()
	p.activeStreams++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.activeStreams--
		p.mu.Unlock()
	}()

	// 创建较长超时的上下文（直播流持续时间长）
	ctx, cancel := context.WithTimeout(r.Context(), p.streamTimeout)
	defer cancel()

	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodGet, originalURL, nil)
	if err != nil {
		http.Error(w, "build request error", http.StatusInternalServerError)
		return
	}

	// 复制关键 header（不转发 Range：直播流按字节范围请求会导致上游返回残缺数据）
	for _, h := range []string{"User-Agent", "Accept", "Referer"} {
		if v := r.Header.Get(h); v != "" {
			upstreamReq.Header.Set(h, v)
		}
	}

	// 该 client 用于直播流：不限制整体时长，只限制等待响应头的时间
	resp, err := p.client.Do(upstreamReq)
	if err != nil {
		fmt.Printf("[stream] upstream error %s: %v\n", originalURL, err)
		http.Error(w, fmt.Sprintf("upstream error: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// HLS（m3u8）需要重写分片地址，否则分片仍指向原始 ISP 地址，
	// 跨网客户端无法访问。
	br := bufio.NewReader(resp.Body)
	isHLS := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "mpegurl") ||
		strings.Contains(strings.ToLower(originalURL), ".m3u8")
	if !isHLS {
		if head, perr := br.Peek(7); perr == nil && string(head) == "#EXTM3U" {
			isHLS = true
		}
	}
	if isHLS {
		serveRewrittenHLS(w, r, resp, br)
		return
	}

	// 复制响应头，跳过逐跳头与长度相关字段（直播流需由本端重新分块）
	for k, v := range resp.Header {
		if hopByHopHeader(k) {
			continue
		}
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "video/mp2t")
	}
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("X-Proxy", "iptv-zt-bridge")
	w.WriteHeader(resp.StatusCode)

	// 流式复制
	copyStream(w, br)
}

var uriAttrRe = regexp.MustCompile(`URI="([^"]*)"`)

// serveRewrittenHLS 把 m3u8 中的分片/子列表地址重写为本代理地址
func serveRewrittenHLS(w http.ResponseWriter, r *http.Request, resp *http.Response, body io.Reader) {
	raw, err := io.ReadAll(io.LimitReader(body, 8<<20))
	if err != nil {
		http.Error(w, "read playlist error", http.StatusBadGateway)
		return
	}

	token := r.URL.Query().Get("token")
	path := r.URL.Path
	prefix := ""
	if i := strings.LastIndex(path, "/"); i > 0 {
		prefix = path[:i]
	}

	base := resp.Request.URL
	resolve := func(ref string) string {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			return ref
		}
		u, err := url.Parse(ref)
		if err == nil && u.IsAbs() {
			return m3u.BuildProxyURL(ref, prefix, token)
		}
		if u, err := url.Parse(ref); err == nil {
			abs := base.ResolveReference(u).String()
			return m3u.BuildProxyURL(abs, prefix, token)
		}
		return ref
	}

	var sb strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			sb.WriteString("\n")
		case strings.HasPrefix(trimmed, "#"):
			// 处理 EXT-X-KEY / EXT-X-MAP / EXT-X-MEDIA 中的 URI="..."
			sb.WriteString(uriAttrRe.ReplaceAllStringFunc(line, func(m string) string {
				sub := uriAttrRe.FindStringSubmatch(m)
				if len(sub) < 2 {
					return m
				}
				return `URI="` + resolve(sub[1]) + `"`
			}))
			sb.WriteString("\n")
		default:
			sb.WriteString(resolve(trimmed))
			sb.WriteString("\n")
		}
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("X-Proxy", "iptv-zt-bridge")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.WriteString(w, sb.String())
}

// hopByHopHeader 判断是否为不应透传的逐跳头 / 长度头
func hopByHopHeader(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length",
		"Content-Encoding", "Alt-Svc":
		return true
	}
	return false
}

// copyStream 持续复制流
func copyStream(dst http.ResponseWriter, src io.Reader) {
	buf := make([]byte, 64*1024)
	flusher, canFlush := dst.(http.Flusher)

	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// ActiveStreams 当前活动流数
func (p *Proxy) ActiveStreams() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.activeStreams
}

// TunnelTest 测试到目标 IP:Port 的连通性
func (p *Proxy) TunnelTest(ctx context.Context, host string, port int) (bool, time.Duration, error) {
	start := time.Now()
	addr := fmt.Sprintf("%s:%d", host, port)

	var dialer net.Dialer
	dialer.Timeout = 5 * time.Second
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false, 0, err
	}
	conn.Close()
	return true, time.Since(start), nil
}
