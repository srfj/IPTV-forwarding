package m3u

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Entry m3u 条目
type Entry struct {
	// 名称（#EXTINF 的名称字段）
	Name string
	// 分组（group-title）
	Group string
	// 原始 URL
	OriginalURL string
	// 处理后的 URL（带 token / 重写代理）
	ProcessedURL string
	// 是否可用（健康检测）
	Alive bool
	// 连续检测失败次数（用于避免单次抖动误杀）
	FailCount int
	// 最后的检测错误
	LastError string
}

// Playlist 完整播放列表
type Playlist struct {
	Entries []*Entry
	// Custom 客户端自定义分类条目：原样下发（不走 token / 本地代理），
	// 排在同步列表之后。每次刷新同步列表时不会被覆盖。
	Custom []*Entry
	mu     sync.RWMutex
}

// Parser m3u 解析器
type Parser struct {
	client *http.Client
}

// NewParser 创建解析器
func NewParser() *Parser {
	return &Parser{
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// ParseURL 从 URL 拉取并解析 m3u
func (p *Parser) ParseURL(ctx context.Context, rawURL string) (*Playlist, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch m3u: %w", err)
	}
	defer resp.Body.Close()

	return p.ParseReader(resp.Body)
}

// ParseFile 从文件解析 m3u
func (p *Parser) ParseFile(path string) (*Playlist, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open m3u file: %w", err)
	}
	defer f.Close()
	return p.ParseReader(f)
}

// ParseReader 从 io.Reader 解析 m3u
func (p *Parser) ParseReader(r io.Reader) (*Playlist, error) {
	scanner := bufio.NewScanner(r)
	// 允许超长行
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, 10*1024*1024)

	pl := &Playlist{}
	var currentEntry *Entry
	groupTitle := ""

	// 分组正则
	groupRe := regexp.MustCompile(`group-title="([^"]*)"`)
	// 名称正则（最后一个 , 之后的内容）
	nameRe := regexp.MustCompile(`,\s*(.*)$`)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "#EXTM3U") {
			// 头部，检查全局 group-title
			if m := groupRe.FindStringSubmatch(line); m != nil {
				groupTitle = m[1]
			}
			continue
		}

		if strings.HasPrefix(line, "#EXTINF:") {
			// 新条目开始，保存上一个
			if currentEntry != nil {
				pl.Entries = append(pl.Entries, currentEntry)
			}

			currentEntry = &Entry{
				Alive: true,
				Group: groupTitle,
			}

			// 解析 group-title
			if m := groupRe.FindStringSubmatch(line); m != nil {
				currentEntry.Group = m[1]
			}

			// 解析名称
			if m := nameRe.FindStringSubmatch(line); m != nil {
				currentEntry.Name = strings.TrimSpace(m[1])
			} else {
				currentEntry.Name = fmt.Sprintf("Channel_%d", len(pl.Entries)+1)
			}
			continue
		}

		// 跳过其他 EXT 标签
		if strings.HasPrefix(line, "#") {
			continue
		}

		// URL 行
		if currentEntry != nil {
			currentEntry.OriginalURL = line
			currentEntry.ProcessedURL = line
			pl.Entries = append(pl.Entries, currentEntry)
			currentEntry = nil
		}
	}

	// 最后一个条目
	if currentEntry != nil {
		pl.Entries = append(pl.Entries, currentEntry)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan m3u: %w", err)
	}

	return pl, nil
}

// ToString 输出为 m3u 文本
func (pl *Playlist) ToString() string {
	return pl.ToStringWithBase("")
}

// ToStringWithBase 输出 m3u；base 形如 http://10.0.0.1:8081，
// 用于把相对路径（/stream/...）补全为绝对地址，便于电视播放器直接加载。
func (pl *Playlist) ToStringWithBase(base string) string {
	pl.mu.RLock()
	defer pl.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")

	// 按分组组织
	groups := map[string][]*Entry{}
	var groupOrder []string

	for _, e := range pl.Entries {
		// 连续多轮检测失败才从对外列表剔除；单次抖动（FailCount=1）仍保留，
		// 避免把能播放的频道误删。
		if !e.Alive && e.FailCount >= 2 {
			continue
		}
		g := e.Group
		if _, exists := groups[g]; !exists {
			groupOrder = append(groupOrder, g)
		}
		groups[g] = append(groups[g], e)
	}

	for _, g := range groupOrder {
		entries := groups[g]
		for _, e := range entries {
			effectiveURL := e.ProcessedURL
			if effectiveURL == "" {
				effectiveURL = e.OriginalURL
			}
			if base != "" && strings.HasPrefix(effectiveURL, "/") {
				effectiveURL = strings.TrimSuffix(base, "/") + effectiveURL
			}

			sb.WriteString(fmt.Sprintf("#EXTINF:-1 group-title=\"%s\",%s\n",
				escapeAttr(g), escapeAttr(e.Name)))
			sb.WriteString(effectiveURL)
			sb.WriteString("\n")
		}
	}

	// 自定义分类整体追加在同步列表之后，地址原样输出（三网通用地址，不经套件）
	for _, e := range pl.Custom {
		if e.OriginalURL == "" {
			continue
		}
		sb.WriteString(fmt.Sprintf("#EXTINF:-1 group-title=\"%s\",%s\n",
			escapeAttr(e.Group), escapeAttr(e.Name)))
		sb.WriteString(e.OriginalURL)
		sb.WriteString("\n")
	}

	return sb.String()
}

// SetCustom 替换自定义分类条目（按传入顺序输出）
func (pl *Playlist) SetCustom(entries []*Entry) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	pl.Custom = entries
}

// CustomCopy 返回自定义分类条目的副本
func (pl *Playlist) CustomCopy() []*Entry {
	pl.mu.RLock()
	defer pl.mu.RUnlock()
	out := make([]*Entry, len(pl.Custom))
	copy(out, pl.Custom)
	return out
}

// escapeAttr 转义属性字符串
func escapeAttr(s string) string {
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// FilterAlive 只保留存活的条目
func (pl *Playlist) FilterAlive() *Playlist {
	pl.mu.RLock()
	defer pl.mu.RUnlock()

	out := &Playlist{}
	for _, e := range pl.Entries {
		if e.Alive {
			out.Entries = append(out.Entries, e)
		}
	}
	return out
}

// Count 返回条目数
func (pl *Playlist) Count() int {
	pl.mu.RLock()
	defer pl.mu.RUnlock()
	return len(pl.Entries)
}

// CountAlive 返回存活条目数
func (pl *Playlist) CountAlive() int {
	pl.mu.RLock()
	defer pl.mu.RUnlock()
	n := 0
	for _, e := range pl.Entries {
		if e.Alive {
			n++
		}
	}
	return n
}

// EntriesCopy 返回所有条目的副本
func (pl *Playlist) EntriesCopy() []*Entry {
	pl.mu.RLock()
	defer pl.mu.RUnlock()
	out := make([]*Entry, len(pl.Entries))
	copy(out, pl.Entries)
	return out
}

// Replace 用新列表替换所有条目
func (pl *Playlist) Replace(newEntries []*Entry) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	pl.Entries = newEntries
}

// ApplyToken 给所有 URL 添加 Token（Server 模式）
func (pl *Playlist) ApplyToken(token, streamPath string) {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	prefix := strings.TrimSuffix(streamPath, "/")
	for _, e := range pl.Entries {
		e.ProcessedURL = buildServerURL(e.OriginalURL, token, prefix)
	}
}

// BuildServerURL 构建服务端对外相对 URL（导出供客户端复用）
func BuildServerURL(original, token, streamPath string) string {
	return buildServerURL(original, token, strings.TrimSuffix(streamPath, "/"))
}

// buildServerURL 构建服务端对外 URL
func buildServerURL(original, token, prefix string) string {
	// 格式: /stream/{base64url(original)}.ts?token=xxx
	// 加 .ts 后缀是为了兼容按扩展名判断流类型的电视播放器
	encoded := base64URL(original)
	return fmt.Sprintf("%s/%s.ts?token=%s", prefix, encoded, token)
}

// ApplyLocalProxy 重写为本地代理 URL（Client 模式，相对路径）
func (pl *Playlist) ApplyLocalProxy(streamPath, token string) {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	prefix := strings.TrimSuffix(streamPath, "/")
	for _, e := range pl.Entries {
		e.ProcessedURL = BuildProxyURL(e.OriginalURL, prefix, token)
	}
}

// BuildProxyURL 构建客户端本地代理相对 URL
func BuildProxyURL(original, streamPath, token string) string {
	prefix := strings.TrimSuffix(streamPath, "/")
	encoded := base64URL(original)
	return fmt.Sprintf("%s/%s.ts?token=%s", prefix, encoded, token)
}

// DecodeEncodedURL 解析编码后的 URL
func DecodeEncodedURL(encoded string) (string, error) {
	decoded, err := base64URIDecode(encoded)
	if err != nil {
		return "", fmt.Errorf("decode url: %w", err)
	}
	return string(decoded), nil
}

// StripOriginalFromServerURL 从服务端 URL 提取原始源
func StripOriginalFromServerURL(rawURL string) (string, error) {
	// rawURL 形如: /stream/{base64}?token=xxx
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	// 路径最后一段应该是编码后的 URL
	path := strings.Trim(u.Path, "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 {
		return "", fmt.Errorf("invalid path")
	}

	encoded := parts[len(parts)-1]
	return DecodeEncodedURL(encoded)
}

// --- Base64 URL 编码 ---

func base64URL(s string) string {
	return base64URLEncode([]byte(s))
}

func base64URLEncode(data []byte) string {
	var buf []byte
	// 简单 base64 url-safe 编码
	const enc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

	for i := 0; i < len(data); i += 3 {
		var b0, b1, b2 byte
		b0 = data[i]
		if i+1 < len(data) {
			b1 = data[i+1]
		}
		if i+2 < len(data) {
			b2 = data[i+2]
		}

		buf = append(buf,
			enc[b0>>2],
			enc[((b0&0x3)<<4)|(b1>>4)],
		)

		if i+1 < len(data) {
			buf = append(buf, enc[((b1&0xf)<<2)|(b2>>6)])
		} else {
			buf = append(buf, '=')
		}

		if i+2 < len(data) {
			buf = append(buf, enc[b2&0x3f])
		} else {
			buf = append(buf, '=')
		}
	}
	return string(buf)
}

func base64URIDecode(s string) ([]byte, error) {
	const enc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	indexOf := func(c byte) int {
		for i := 0; i < len(enc); i++ {
			if enc[i] == c {
				return i
			}
		}
		return -1
	}

	s = strings.TrimRight(s, "=")
	var out []byte

	for i := 0; i < len(s); i += 4 {
		var vals [4]int
		for j := 0; j < 4; j++ {
			if i+j < len(s) {
				vals[j] = indexOf(s[i+j])
				if vals[j] < 0 {
					return nil, fmt.Errorf("invalid base64 char")
				}
			} else {
				vals[j] = 0
			}
		}

		out = append(out, byte((vals[0]<<2)|(vals[1]>>4)))
		if i+2 < len(s) {
			out = append(out, byte(((vals[1]&0xf)<<4)|(vals[2]>>2)))
		}
		if i+3 < len(s) {
			out = append(out, byte(((vals[2]&0x3)<<6)|vals[3]))
		}
	}
	return out, nil
}

// CheckAlive 检测单个源是否存活。
//
// 关键点（历史 bug）：IPTV 源普遍不支持 HTTP HEAD，很多源对 HEAD 直接返回
// 405/400 或断开连接；而 rtp://、udp://、igmp://、rtsp:// 这些地址根本无法用
// HTTP 探测。旧实现一律把上述情况判为“失效”，导致大量能播放的频道被过滤掉。
// 现在只对明确不可用的信号（404/410/5xx/连接失败）判死，其余一律按可用处理。
func (p *Parser) CheckAlive(ctx context.Context, rawURL string, timeout time.Duration) error {
	// IPTV 首包普遍偏慢，5 秒级超时会造成大量误判
	if timeout < 8*time.Second {
		timeout = 8 * time.Second
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}

	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		// 可以探测
	case "rtp", "udp", "igmp", "rtsp", "rtmp", "rtmps", "rtp2httpd":
		// 组播/其他协议：无法用 HTTP 判断存活，视为可用，交给实际播放去验证
		return nil
	default:
		return fmt.Errorf("unsupported scheme %q", u.Scheme)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := &http.Client{Timeout: timeout}

	// 先用 HEAD（开销小）；HEAD 不通或源不支持方法时再退回 GET
	resp, err := p.probeHTTP(ctx, client, http.MethodHead, rawURL)
	if err != nil {
		resp, err = p.probeHTTP(ctx, client, http.MethodGet, rawURL)
		if err != nil {
			return err
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		resp.Body.Close()
		resp, err = p.probeHTTP(ctx, client, http.MethodGet, rawURL)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
	}

	return classifyStatus(resp.StatusCode)
}

// probeHTTP 发起一次探测请求，不读取响应体（直播流不会立刻给 body）
func (p *Parser) probeHTTP(ctx context.Context, client *http.Client, method, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "IPTV-ZT-Bridge/health")
	// 只请求首个字节，避免服务端推流把探测连接长时间占用
	req.Header.Set("Range", "bytes=0-1")
	return client.Do(req)
}

// classifyStatus 判定 HTTP 状态码是否代表“源不可用”。
// 401/403/405 等表示主机在线、只是拒绝探测，不等于节目不可播。
func classifyStatus(code int) error {
	switch {
	case code >= 200 && code < 400:
		return nil
	case code == http.StatusNotFound, code == http.StatusGone:
		return fmt.Errorf("status %d", code)
	case code >= 500:
		return fmt.Errorf("status %d", code)
	default:
		// 其余 4xx：源存在但拒绝了探测（鉴权/方法/限流），不判死
		return nil
	}
}

// HealthChecker 健康检测器
type HealthChecker struct {
	parser  *Parser
	client  *http.Client
	timeout time.Duration
	workers int
}

// NewHealthChecker 创建健康检测器
func NewHealthChecker(timeout time.Duration) *HealthChecker {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &HealthChecker{
		parser:  NewParser(),
		client:  &http.Client{Timeout: timeout},
		timeout: timeout,
		workers: 10,
	}
}

// CheckAll 批量检测。
// 注意：不在这里长时间持有 playlist 锁——网络探测可能耗时数十秒，
// 持锁会导致 m3u / API 请求全部卡住。
func (hc *HealthChecker) CheckAll(ctx context.Context, pl *Playlist) {
	entries := pl.EntriesCopy()
	if len(entries) == 0 {
		return
	}

	if hc.workers <= 0 {
		hc.workers = 10
	}

	sem := make(chan struct{}, hc.workers)
	var wg sync.WaitGroup

	// 先在副本上并发探测，结果写回局部数组，最后统一加锁回写，
	// 既避免长时间持锁，也避免与 m3u/API 读取产生数据竞争。
	type result struct {
		alive     bool
		failCount int
		lastErr   string
	}
	results := make([]result, len(entries))

	for i, e := range entries {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, entry *Entry) {
			defer wg.Done()
			defer func() { <-sem }()

			// 跳过 m3u 类型（列表地址本身不代表单个频道）
			if strings.HasSuffix(strings.ToLower(entry.OriginalURL), ".m3u") {
				results[idx] = result{alive: true}
				return
			}

			// 检测源地址或已处理的地址
			checkURL := entry.OriginalURL
			if checkURL == "" {
				checkURL = entry.ProcessedURL
			}

			// 重试 2 次：IPTV 首包慢、瞬时抖动很常见，单次失败不足以判死
			var err error
			for attempt := 0; attempt < 3; attempt++ {
				if err = hc.parser.CheckAlive(ctx, checkURL, hc.timeout); err == nil {
					break
				}
				if ctx.Err() != nil {
					return
				}
				time.Sleep(time.Duration(attempt+1) * 400 * time.Millisecond)
			}

			if err == nil {
				results[idx] = result{alive: true}
				return
			}

			// 连续 2 轮检测都失败才判定失效，避免把能播的频道误杀
			fails := entry.FailCount + 1
			results[idx] = result{
				alive:     fails < 2,
				failCount: fails,
				lastErr:   err.Error(),
			}
		}(i, e)
	}

	wg.Wait()

	pl.mu.Lock()
	for i, e := range entries {
		r := results[i]
		// 因 ctx 取消而未得出结论的条目保持原状态
		if r.lastErr == "" && !r.alive {
			continue
		}
		e.Alive = r.alive
		e.FailCount = r.failCount
		e.LastError = r.lastErr
	}
	pl.mu.Unlock()
}
