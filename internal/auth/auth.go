package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TokenAuth Token 鉴权
type TokenAuth struct {
	token string
}

// NewTokenAuth 创建 Token 鉴权器
func NewTokenAuth(token string) *TokenAuth {
	return &TokenAuth{token: token}
}

// Validate 验证请求携带的 Token
func (t *TokenAuth) Validate(r *http.Request) bool {
	// 从 query 参数获取
	if token := r.URL.Query().Get("token"); token != "" {
		return token == t.token
	}
	// 从 header 获取
	if token := r.Header.Get("X-Token"); token != "" {
		return token == t.token
	}
	// 从 Authorization header 获取
	if auth := r.Header.Get("Authorization"); auth != "" {
		if strings.HasPrefix(auth, "Bearer ") {
			return strings.TrimPrefix(auth, "Bearer ") == t.token
		}
	}
	return false
}

// Middleware HTTP 中间件
func (t *TokenAuth) Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !t.Validate(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// ApplyToURL 将 token 附加到 URL
func (t *TokenAuth) ApplyToURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("token", t.token)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// --- WebUI Session 鉴权 ---

// Session WebUI 会话
type Session struct {
	Token     string    `json:"token"`
	User      string    `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// SessionManager 会话管理器
type SessionManager struct {
	mu        sync.RWMutex
	sessions  map[string]*Session
	sessionKey string
	ttl       time.Duration
}

// NewSessionManager 创建会话管理器
func NewSessionManager(sessionKey string, ttl time.Duration) *SessionManager {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &SessionManager{
		sessions:   make(map[string]*Session),
		sessionKey: sessionKey,
		ttl:        ttl,
	}
}

// GenerateToken 生成随机 token
func GenerateToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Create 创建会话
func (sm *SessionManager) Create(user string) *Session {
	token := GenerateToken()
	now := time.Now()
	s := &Session{
		Token:     token,
		User:      user,
		CreatedAt: now,
		ExpiresAt: now.Add(sm.ttl),
	}

	sm.mu.Lock()
	sm.sessions[token] = s
	sm.mu.Unlock()

	return s
}

// Validate 验证会话
func (sm *SessionManager) Validate(token string) (*Session, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	s, ok := sm.sessions[token]
	if !ok {
		return nil, false
	}
	if time.Now().After(s.ExpiresAt) {
		return nil, false
	}
	return s, true
}

// Revoke 撤销会话
func (sm *SessionManager) Revoke(token string) {
	sm.mu.Lock()
	delete(sm.sessions, token)
	sm.mu.Unlock()
}

// CleanExpired 清理过期会话
func (sm *SessionManager) CleanExpired() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	for k, v := range sm.sessions {
		if now.After(v.ExpiresAt) {
			delete(sm.sessions, k)
		}
	}
}

// AuthSession Middleware WebUI 会话鉴权
func (sm *SessionManager) AuthSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := ""

		// 从 cookie 获取
		if c, err := r.Cookie("session"); err == nil {
			token = c.Value
		}
		// 从 header 获取
		if token == "" {
			token = r.Header.Get("X-Session")
		}

		if _, ok := sm.Validate(token); !ok {
			// API 请求返回 JSON 错误
			if strings.HasPrefix(r.URL.Path, "/api/") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
				return
			}
			// 其他重定向到登录页
			http.Redirect(w, r, "/login.html", http.StatusFound)
			return
		}

		next(w, r)
	}
}

// BasicAuth WebUI 基础鉴权（登录接口）
func (sm *SessionManager) BasicAuth(username, password, user, pass string) error {
	if username == "" || password == "" {
		return fmt.Errorf("credentials required")
	}
	if username != user || password != pass {
		return fmt.Errorf("invalid credentials")
	}
	return nil
}
