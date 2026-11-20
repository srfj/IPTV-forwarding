package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"

	"github.com/iptv-zt-bridge/internal/auth"
	"github.com/iptv-zt-bridge/internal/config"
	"github.com/iptv-zt-bridge/internal/m3u"
	"github.com/iptv-zt-bridge/internal/proxy"
	"github.com/iptv-zt-bridge/internal/rtp2httpd"
	"github.com/iptv-zt-bridge/internal/zerotier"
)

// Server Web 服务器
type Server struct {
	cfg        *config.Config
	zt         *zerotier.Manager
	rtp        *rtp2httpd.Manager
	tokenAuth  *auth.TokenAuth
	sessionMgr *auth.SessionManager
	proxy      *proxy.Proxy
	router     *mux.Router
	httpServer *http.Server
	refreshFn  func()

	tokMu     sync.RWMutex
	discState string
	playlist  *m3u.Playlist

	discMu   sync.Mutex
	discSeen map[string]int64

	version string
}

// SetVersion 设置版本号，供 WebUI 展示与排查
func (s *Server) SetVersion(v string) {
	s.version = v
}

// NewServer 创建 Web 服务器
func NewServer(cfg *config.Config, ztMgr *zerotier.Manager, streamProxy *proxy.Proxy, rtpMgr *rtp2httpd.Manager) *Server {
	s := &Server{
		cfg:        cfg,
		zt:         ztMgr,
		rtp:        rtpMgr,
		tokenAuth:  auth.NewTokenAuth(cfg.Token),
		sessionMgr: auth.NewSessionManager(cfg.Web.SessionKey, 24*time.Hour),
		proxy:      streamProxy,
		router:     mux.NewRouter(),
	}
	s.setupRoutes()
	return s
}

// noCache 给静态资源加禁止缓存头，避免升级后浏览器仍用旧页面
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		h.ServeHTTP(w, r)
	})
}

// setupRoutes 设置路由
func (s *Server) setupRoutes() {
	r := s.router

	// 静态文件（WebUI），禁用缓存，确保升级后立即使用新页面
	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/",
		noCache(http.FileServer(http.Dir(s.cfg.Runtime.WebRoot)))))

	// 登录页
	r.HandleFunc("/login.html", s.loginPage).Methods("GET")
	r.HandleFunc("/api/login", s.login).Methods("POST")

	// 根路径重定向
	r.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login.html", http.StatusFound)
	}).Methods("GET")

	// 对外端点（m3u / 流 / 服务发现）同时挂在 WebUI 端口上。
	// 飞牛 fnOS 等系统只放行 manifest 中声明的 service_port，
	// 单独监听的 8081 会被防火墙丢弃，导致客户端扫描不到服务端。
	publicPrefix := strings.TrimSuffix(s.getStreamPathPrefix(), "/")
	r.HandleFunc("/api/discover", s.handleDiscover).Methods("GET")
	r.HandleFunc(publicPrefix+"/{encoded}", s.authed(s.handleStream)).Methods("GET", "HEAD")
	r.HandleFunc(s.getM3UPath(), s.authed(s.handleM3U)).Methods("GET")

	// --- API ---
	api := r.PathPrefix("/api").Subrouter()
	api.Use(s.sessionMiddleware)

	api.HandleFunc("/status", s.apiStatus).Methods("GET")
	api.HandleFunc("/config", s.apiGetConfig).Methods("GET")
	api.HandleFunc("/config", s.apiUpdateConfig).Methods("PUT")
	api.HandleFunc("/zt/status", s.apiZTStatus).Methods("GET")
	api.HandleFunc("/zt/start", s.apiZTStart).Methods("POST")
	api.HandleFunc("/zt/stop", s.apiZTStop).Methods("POST")
	api.HandleFunc("/zt/join", s.apiZTJoin).Methods("POST")
	api.HandleFunc("/zt/leave", s.apiZTLeave).Methods("POST")
	api.HandleFunc("/playlist", s.apiPlaylist).Methods("GET")
	api.HandleFunc("/playlist/refresh", s.apiRefreshPlaylist).Methods("POST")
	api.HandleFunc("/health", s.apiHealth).Methods("GET")
	api.HandleFunc("/logs", s.apiLogs).Methods("GET")
	api.HandleFunc("/diagnostics", s.apiDiagnostics).Methods("GET")
	api.HandleFunc("/rtp2httpd/status", s.apiRTPStatus).Methods("GET")
	api.HandleFunc("/rtp2httpd/start", s.apiRTPStart).Methods("POST")
	api.HandleFunc("/rtp2httpd/stop", s.apiRTPStop).Methods("POST")
	api.HandleFunc("/service/start", s.apiServiceStart).Methods("POST")
	api.HandleFunc("/service/stop", s.apiServiceStop).Methods("POST")
	api.HandleFunc("/upload/m3u", s.apiUploadM3U).Methods("POST")
	api.HandleFunc("/logout", s.apiLogout).Methods("POST")
	api.HandleFunc("/api-key", s.apiRegenToken).Methods("POST")
}

// PublicHandler 对外服务（m3u / 流代理 / 服务发现），绑定到 Server/Client 的 ListenAddr
func (s *Server) PublicHandler() http.Handler {
	m := mux.NewRouter()
	prefix := strings.TrimSuffix(s.getStreamPathPrefix(), "/")
	m.HandleFunc(prefix+"/{encoded}", s.authed(s.handleStream)).Methods("GET", "HEAD")
	m.HandleFunc(s.getM3UPath(), s.authed(s.handleM3U)).Methods("GET")
	// 局域网/ZeroTier 内的客户端自动发现（仅服务端有效）
	m.HandleFunc("/api/discover", s.handleDiscover).Methods("GET")
	return m
}

// authed 每次请求时取当前 Token 鉴权器，保证 Token 运行时变更后立即生效
func (s *Server) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.tokMu.RLock()
		ta := s.tokenAuth
		s.tokMu.RUnlock()
		ta.Middleware(h).ServeHTTP(w, r)
	}
}

// SetToken 运行时更新 Token（客户端自动发现服务端 Token 后调用）
func (s *Server) SetToken(tok string) {
	s.tokMu.Lock()
	s.cfg.Token = tok
	s.tokenAuth = auth.NewTokenAuth(tok)
	s.tokMu.Unlock()
}

// SetDiscoverState 记录客户端自动发现进度（供 WebUI 展示）
func (s *Server) SetDiscoverState(st string) {
	s.tokMu.Lock()
	s.discState = st
	s.tokMu.Unlock()
}

func (s *Server) getDiscoverState() string {
	s.tokMu.RLock()
	defer s.tokMu.RUnlock()
	return s.discState
}

func (s *Server) getStreamPathPrefix() string {
	if s.cfg.Mode == config.ModeClient {
		return s.cfg.Client.StreamPath
	}
	return s.cfg.Server.StreamPath
}

func (s *Server) getM3UPath() string {
	if s.cfg.Mode == config.ModeClient {
		return s.cfg.Client.M3UPath
	}
	return s.cfg.Server.M3UPath
}

// sessionMiddleware 会话中间件
func (s *Server) sessionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if c, err := r.Cookie("session"); err == nil {
			token = c.Value
		}
		if token == "" {
			token = r.Header.Get("X-Session")
		}
		if _, ok := s.sessionMgr.Validate(token); !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Start 启动 HTTP 服务
func (s *Server) Start(ctx context.Context) error {
	s.httpServer = &http.Server{
		Addr:    s.cfg.Web.ListenAddr,
		Handler: s.router,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(shutdownCtx)
	}()

	fmt.Printf("[web] listening on %s\n", s.cfg.Web.ListenAddr)
	return s.httpServer.ListenAndServe()
}

// SetPlaylist 设置播放列表
func (s *Server) SetPlaylist(pl *m3u.Playlist) {
	s.playlist = pl
	// 客户端：每次同步列表更新后，把自定义分类重新挂在后面，
	// 保证两者一起下发给播放器。
	if pl != nil && s.cfg.GetMode() == config.ModeClient {
		pl.SetCustom(BuildCustomEntries(s.cfg.Client.CustomGroups))
	}
}

// OnRefresh 注册手动刷新回调
func (s *Server) OnRefresh(fn func()) {
	s.refreshFn = fn
}

// --- Handlers ---

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	// 检查是否已登录
	if c, err := r.Cookie("session"); err == nil {
		if _, ok := s.sessionMgr.Validate(c.Value); ok {
			http.Redirect(w, r, "/static/index.html", http.StatusFound)
			return
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(loginHTML))
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}

	if err := s.sessionMgr.BasicAuth(body.Username, body.Password,
		s.cfg.Web.Username, s.cfg.Web.Password); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}

	sess := s.sessionMgr.Create(body.Username)
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    sess.Token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   86400,
	})

	writeJSON(w, http.StatusOK, map[string]string{
		"session": sess.Token,
		"expires": sess.ExpiresAt.Format(time.RFC3339),
	})
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("session"); err == nil {
		s.sessionMgr.Revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:   "session",
		Path:   "/",
		MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	ztStatus := s.zt.Status()
	resp := map[string]interface{}{
		"mode":      s.cfg.Mode,
		"version":   s.version,
		"zt_status": ztStatus,
		"api_token": s.cfg.Token != "",
		"timestamp": time.Now().UTC(),
		"playlist":  s.playlistInfo(),
		"streaming": map[string]interface{}{
			"active_proxies": s.proxy.ActiveStreams(),
		},
	}
	if s.rtp != nil {
		resp["rtp2httpd"] = s.rtp.Status()
	}
	if s.cfg.GetMode() == config.ModeClient {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil && h != "" {
			host = h
		}
		port := "8086"
		if _, p, err := net.SplitHostPort(s.cfg.Client.ListenAddr); err == nil && p != "" && s.cfg.Client.ListenAddr != "0.0.0.0:8086" {
			port = p
		}
		resp["client"] = map[string]interface{}{
			"server_addr": s.cfg.Client.ServerAddr,
			"discover":    s.getDiscoverState(),
			"local_m3u": fmt.Sprintf("http://%s:%s%s?token=%s",
				host, port, s.cfg.Client.M3UPath, s.cfg.Token),
		}
	}
	resp["service"] = map[string]interface{}{
		"running": s.serviceRunning(),
		"missing": s.missingParams(),
	}
	writeJSON(w, http.StatusOK, resp)
}

// serviceRunning 整套服务是否已启动（ZeroTier + 内置 rtp2httpd）
func (s *Server) serviceRunning() bool {
	if !s.zt.Status().Running {
		return false
	}
	if s.cfg.GetMode() == config.ModeServer {
		return s.rtp != nil && s.rtp.Status().Running
	}
	return true
}

// missingParams 返回尚未填写的必要参数
func (s *Server) missingParams() []string {
	missing := []string{}

	if strings.TrimSpace(s.cfg.ZeroTier.NetworkID) == "" {
		missing = append(missing, "ZeroTier 网络 ID")
	}

	if s.cfg.GetMode() == config.ModeServer {
		src := strings.TrimSpace(s.cfg.RTP2HTTPD.SourceM3U)
		switch {
		case src == "":
			missing = append(missing, "频道源 M3U（请上传）")
		case strings.HasPrefix(src, "http://"), strings.HasPrefix(src, "https://"):
			// 远程地址无需校验本地文件
		default:
			p := strings.TrimPrefix(src, "file://")
			if _, err := os.Stat(p); err != nil {
				missing = append(missing, "频道源 M3U 文件（不存在，请重新上传）")
			}
		}
		if strings.TrimSpace(s.cfg.RTP2HTTPD.ListenAddr) == "" {
			missing = append(missing, "rtp2httpd 监听地址")
		}
		if strings.TrimSpace(s.cfg.Server.ListenAddr) == "" {
			missing = append(missing, "对外监听地址")
		}
	} else {
		// 客户端只需 ZeroTier 网络 ID，服务端地址与 Token 由自动发现获取
		if strings.TrimSpace(s.cfg.Client.ListenAddr) == "" {
			missing = append(missing, "本地监听地址")
		}
	}

	return missing
}

func (s *Server) playlistInfo() map[string]interface{} {
	if s.playlist == nil {
		return map[string]interface{}{"total": 0, "alive": 0}
	}
	return map[string]interface{}{
		"total": s.playlist.Count(),
		"alive": s.playlist.CountAlive(),
	}
}

func (s *Server) apiGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.cfg.Get())
}

func (s *Server) apiUpdateConfig(w http.ResponseWriter, r *http.Request) {
	var patch map[string]interface{}
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}

	if err := s.cfg.Update(patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// 保存到文件
	if s.cfg.Runtime.ConfigFile != "" {
		if err := s.cfg.Save(s.cfg.Runtime.ConfigFile); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save failed"})
			return
		}
	}

	// Token 可能被修改，重建鉴权器使其立即生效
	s.SetToken(s.cfg.Token)

	// 自定义分类可能被修改，立即重新生成（仅客户端生效）
	if s.playlist != nil && s.cfg.GetMode() == config.ModeClient {
		s.playlist.SetCustom(BuildCustomEntries(s.cfg.Client.CustomGroups))
	}

	writeJSON(w, http.StatusOK, s.cfg.Get())
}

// BuildCustomEntries 把客户端自定义分类转换成播放列表条目。
// 每个分类是一整段文本，支持三种写法（可混用）：
//
//	#EXTINF:-1,频道名        <- 标准 m3u 片段
//	http://...
//	频道名,http://...        <- 一行一个
//	http://...
//
// 条目地址原样下发，不走 token / 本地代理。
func BuildCustomEntries(groups []config.CustomGroup) []*m3u.Entry {
	var out []*m3u.Entry
	for _, g := range groups {
		name := strings.TrimSpace(g.Name)
		if name == "" {
			continue
		}
		pending := ""
		for _, raw := range strings.Split(g.Content, "\n") {
			line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
			if line == "" || strings.HasPrefix(line, "#EXTM3U") {
				continue
			}
			if strings.HasPrefix(line, "#EXTINF") {
				// 取最后一个逗号之后的内容作为频道名
				if i := strings.LastIndex(line, ","); i >= 0 {
					pending = strings.TrimSpace(line[i+1:])
				}
				continue
			}
			if strings.HasPrefix(line, "#") {
				continue
			}

			title := pending
			pending = ""
			url := line
			if !hasScheme(url) {
				// 「频道名,地址」写法
				if i := strings.LastIndex(line, ","); i >= 0 {
					if u := strings.TrimSpace(line[i+1:]); hasScheme(u) {
						if t := strings.TrimSpace(line[:i]); t != "" {
							title = t
						}
						url = u
					}
				}
			}
			if !hasScheme(url) {
				continue
			}
			if title == "" {
				title = url
			}
			out = append(out, &m3u.Entry{
				Name:        title,
				Group:       name,
				OriginalURL: url,
				Alive:       true,
			})
		}
	}
	return out
}

func hasScheme(u string) bool {
	l := strings.ToLower(u)
	for _, p := range []string{"http://", "https://", "rtsp://", "rtmp://", "udp://", "rtp://"} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

func (s *Server) apiZTStatus(w http.ResponseWriter, r *http.Request) {
	status := s.zt.Status()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": status,
		"hint":   s.zt.GetMembershipAuthHint(),
	})
}

func (s *Server) apiZTStart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := s.zt.Start(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

func (s *Server) apiZTStop(w http.ResponseWriter, r *http.Request) {
	if err := s.zt.Stop(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (s *Server) apiZTJoin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		NetworkID string `json:"network_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	if body.NetworkID != "" {
		s.cfg.ZeroTier.NetworkID = body.NetworkID
	}

	if err := s.zt.JoinNetwork(s.cfg.ZeroTier.NetworkID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "joined",
		"hint":   s.zt.GetMembershipAuthHint(),
	})
}

func (s *Server) apiZTLeave(w http.ResponseWriter, r *http.Request) {
	if err := s.zt.LeaveNetwork(s.cfg.ZeroTier.NetworkID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "left"})
}

func (s *Server) apiPlaylist(w http.ResponseWriter, r *http.Request) {
	if s.playlist == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"entries": []interface{}{}})
		return
	}

	entries := make([]map[string]interface{}, 0, len(s.playlist.EntriesCopy()))
	for _, e := range s.playlist.EntriesCopy() {
		entries = append(entries, map[string]interface{}{
			"name":          e.Name,
			"group":         e.Group,
			"original_url":  e.OriginalURL,
			"processed_url": e.ProcessedURL,
			"alive":         e.Alive,
			"last_error":    e.LastError,
		})
	}
	// 自定义分类排在同步列表之后
	for _, e := range s.playlist.CustomCopy() {
		entries = append(entries, map[string]interface{}{
			"name":          e.Name,
			"group":         e.Group,
			"original_url":  e.OriginalURL,
			"processed_url": e.OriginalURL,
			"alive":         true,
			"custom":        true,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"count":   len(entries),
		"entries": entries,
	})
}

func (s *Server) apiRefreshPlaylist(w http.ResponseWriter, r *http.Request) {
	if s.refreshFn != nil {
		s.refreshFn()
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "refresh_requested"})
}

func (s *Server) apiHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"uptime":       time.Now().Unix(),
		"proxy_active": s.proxy.ActiveStreams(),
		"zt_running":   s.zt.Status().Running,
	})
}

func (s *Server) apiRTPStatus(w http.ResponseWriter, r *http.Request) {
	if s.rtp == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false, "running": false})
		return
	}
	writeJSON(w, http.StatusOK, s.rtp.Status())
}

func (s *Server) apiRTPStart(w http.ResponseWriter, r *http.Request) {
	if s.rtp == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "当前套件为客户端，不包含 rtp2httpd"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := s.rtp.Start(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := s.rtp.WaitReady(ctx, 15*time.Second); err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "started",
			"warning": err.Error(),
			"detail":  s.rtp.Status(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "started", "detail": s.rtp.Status()})
}

func (s *Server) apiRTPStop(w http.ResponseWriter, r *http.Request) {
	if s.rtp == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "当前套件为客户端，不包含 rtp2httpd"})
		return
	}
	s.rtp.Stop()
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

// apiServiceStart 一键启动整套服务（内置 rtp2httpd + ZeroTier）
func (s *Server) apiServiceStart(w http.ResponseWriter, r *http.Request) {
	if missing := s.missingParams(); len(missing) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":   "缺少必要参数",
			"missing": missing,
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	if s.cfg.GetMode() == config.ModeServer && s.rtp != nil {
		if err := s.rtp.Start(ctx); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"error":  "内置 rtp2httpd 启动失败：" + err.Error(),
				"detail": s.rtp.Status(),
			})
			return
		}
		if err := s.rtp.WaitReady(ctx, 20*time.Second); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"error":  "内置 rtp2httpd 未就绪：" + err.Error(),
				"detail": s.rtp.Status(),
			})
			return
		}
		s.cfg.Server.SourceM3U = s.rtp.PlaylistURL()
	}

	if err := s.zt.Start(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "ZeroTier 启动失败：" + err.Error(),
		})
		return
	}

	if s.refreshFn != nil {
		go s.refreshFn()
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "running",
		"hint":   s.zt.GetMembershipAuthHint(),
	})
}

// apiServiceStop 一键停止整套服务
func (s *Server) apiServiceStop(w http.ResponseWriter, r *http.Request) {
	if s.rtp != nil {
		s.rtp.Stop()
	}
	if err := s.zt.Stop(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

// apiUploadM3U 上传运营商频道源 M3U（保存到数据目录并更新配置）
func (s *Server) apiUploadM3U(w http.ResponseWriter, r *http.Request) {
	const maxUpload = 20 << 20 // 20MB

	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "上传失败：文件过大或请求格式错误"})
		return
	}

	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请选择要上传的 M3U 文件"})
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxUpload))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "读取文件失败：" + err.Error()})
		return
	}
	if len(bytes.TrimSpace(data)) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "文件内容为空"})
		return
	}
	if !bytes.Contains(data, []byte("#EXTM3U")) && !bytes.Contains(data, []byte("#EXTINF")) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "不是有效的 M3U 文件（缺少 #EXTM3U / #EXTINF）"})
		return
	}

	dir := filepath.Join(s.cfg.Runtime.DataDir, "uploads")
	if err := os.MkdirAll(dir, 0755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "创建上传目录失败：" + err.Error()})
		return
	}
	dst := filepath.Join(dir, "channels.m3u")
	if err := os.WriteFile(dst, data, 0644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存文件失败：" + err.Error()})
		return
	}

	s.cfg.RTP2HTTPD.SourceM3U = dst
	if s.cfg.Runtime.ConfigFile != "" {
		_ = s.cfg.Save(s.cfg.Runtime.ConfigFile)
	}

	// 服务已运行时重启 rtp2httpd，让新频道源立即生效
	restarted := false
	if s.rtp != nil && s.rtp.Status().Running {
		s.rtp.Stop()
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		defer cancel()
		if err := s.rtp.Start(ctx); err == nil {
			_ = s.rtp.WaitReady(ctx, 20*time.Second)
			restarted = true
			if s.refreshFn != nil {
				go s.refreshFn()
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"path":      dst,
		"name":      hdr.Filename,
		"size":      len(data),
		"restarted": restarted,
	})
}

func (s *Server) apiRegenToken(w http.ResponseWriter, r *http.Request) {
	newToken := auth.GenerateToken()
	s.SetToken(newToken)

	if s.cfg.Runtime.ConfigFile != "" {
		_ = s.cfg.Save(s.cfg.Runtime.ConfigFile)
	}

	writeJSON(w, http.StatusOK, map[string]string{"token": newToken})
}

// --- 流 / m3u 处理 ---

// handleDiscover 服务端信息发现（客户端加入同一 ZeroTier 网络后自动调用）
func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	if s.cfg.GetMode() != config.ModeServer {
		http.NotFound(w, r)
		return
	}
	s.logDiscoverOnce(r.RemoteAddr)
	if !s.sourceAllowed(r.RemoteAddr) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"app":    "IPTV-ZT-Bridge",
		"role":   "server",
		"m3u":    s.getM3UPath(),
		"stream": s.getStreamPathPrefix(),
		"token":  s.cfg.Token,
	})
}

// logDiscoverOnce 记录发现请求来源（同一 IP 30 秒只记一次），
// 用于确认客户端探测包是否真的到达服务端。
func (s *Server) logDiscoverOnce(remote string) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	now := time.Now().Unix()
	s.discMu.Lock()
	if s.discSeen == nil {
		s.discSeen = make(map[string]int64)
	}
	last, ok := s.discSeen[host]
	s.discSeen[host] = now
	s.discMu.Unlock()
	if ok && now-last < 30 {
		return
	}
	fmt.Fprintf(os.Stderr, "[discover] 收到来自 %s 的探测\n", host)
}

// sourceAllowed 仅允许局域网 / 同一 ZeroTier 网段访问发现接口
func (s *Server) sourceAllowed(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	// 公网来源一律拒绝，避免 Token 外泄
	if !ip.IsPrivate() {
		return false
	}
	// 若已获得 ZeroTier 虚拟 IP，则只接受同 /24 的来源
	for _, vip := range s.zt.Status().VirtualAddresses {
		cidr := strings.Split(vip, "/")[0]
		v := net.ParseIP(cidr)
		if v == nil {
			continue
		}
		v4, ip4 := v.To4(), ip.To4()
		if v4 != nil && ip4 != nil && v4[0] == ip4[0] && v4[1] == ip4[1] && v4[2] == ip4[2] {
			return true
		}
	}
	return true
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	encoded := strings.TrimSuffix(vars["encoded"], ".ts")

	s.proxy.HandleStream(w, r, encoded)
}

func (s *Server) handleM3U(w http.ResponseWriter, r *http.Request) {
	if s.playlist == nil {
		http.Error(w, "empty playlist", http.StatusNotFound)
		return
	}

	// 用请求的 Host 补全相对路径，保证电视播放器拿到绝对地址
	base := "http://" + r.Host

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Write([]byte(s.playlist.ToStringWithBase(base)))
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
