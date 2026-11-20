package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

// Mode 运行模式
type Mode string

const (
	ModeServer Mode = "server"
	ModeClient Mode = "client"
)

// Config 全局配置
type Config struct {
	mu sync.RWMutex `yaml:"-" json:"-"`

	// 运行模式（由 SPK 套件类型决定，也可用 -mode 覆盖）
	Mode Mode `yaml:"mode" json:"mode"`

	// ZeroTier 配置
	ZeroTier ZeroTierConfig `yaml:"zerotier" json:"zerotier"`

	// 内置 rtp2httpd 配置（仅服务端）
	RTP2HTTPD RTP2HTTPDConfig `yaml:"rtp2httpd" json:"rtp2httpd"`

	// Server 模式配置
	Server ServerConfig `yaml:"server" json:"server"`

	// Client 模式配置
	Client ClientConfig `yaml:"client" json:"client"`

	// 鉴权 Token
	Token string `yaml:"token" json:"token"`

	// WebUI 配置
	Web WebConfig `yaml:"web" json:"web"`

	// 运行时路径（启动时设置）
	Runtime RuntimePaths `yaml:"-" json:"-"`
}

// ZeroTierConfig ZeroTier 相关配置
type ZeroTierConfig struct {
	NetworkID         string `yaml:"network_id" json:"network_id"`
	DataDir           string `yaml:"data_dir" json:"data_dir"`
	BinaryPath        string `yaml:"binary_path" json:"binary_path"`
	APIToken          string `yaml:"api_token" json:"api_token"`
	APIPort           int    `yaml:"api_port" json:"api_port"`
	EnableTun         bool   `yaml:"enable_tun" json:"enable_tun"`
	FallbackUserspace bool   `yaml:"fallback_userspace" json:"fallback_userspace"`
}

// RTP2HTTPDConfig 内置 rtp2httpd 配置
type RTP2HTTPDConfig struct {
	// 二进制路径（默认与主程序同目录的 rtp2httpd）
	BinaryPath string `yaml:"binary_path" json:"binary_path"`
	// 生成的配置文件路径
	ConfigPath string `yaml:"config_path" json:"config_path"`
	// rtp2httpd 监听地址（本地回环即可，由本套件对外转发）
	ListenAddr string `yaml:"listen_addr" json:"listen_addr"`
	// 运营商原始频道列表（file:// / http:// / https://），支持 rtp:// 组播地址
	SourceM3U string `yaml:"source_m3u" json:"source_m3u"`
	// 附加配置（原样写入 rtp2httpd.conf [global]）
	ExtraConf string `yaml:"extra_conf" json:"extra_conf"`
}

// ServerConfig 服务端配置
type ServerConfig struct {
	// rtp2httpd 输出的 m3u（默认自动指向内置 rtp2httpd）
	SourceM3U string `yaml:"source_m3u" json:"source_m3u"`
	// 对外 HTTP 服务监听地址（可填 ZeroTier 虚拟 IP:端口）
	ListenAddr string `yaml:"listen_addr" json:"listen_addr"`
	// 对外 m3u 路径
	M3UPath string `yaml:"m3u_path" json:"m3u_path"`
	// 流代理路径前缀
	StreamPath string `yaml:"stream_path" json:"stream_path"`
	// 源存活检测间隔（秒）
	HealthCheckInterval int `yaml:"health_check_interval" json:"health_check_interval"`
	// 源存活检测超时（秒）
	HealthCheckTimeout int `yaml:"health_check_timeout" json:"health_check_timeout"`
}

// CustomGroup 客户端自定义分类，追加在原同步列表之后一起下发。
// 内容为整段文本，支持 m3u 片段（#EXTINF + 地址）或每行「频道名,地址」/ 纯地址。
type CustomGroup struct {
	Name    string `yaml:"name" json:"name"`
	Content string `yaml:"content" json:"content"`
}

// ClientConfig 客户端配置
type ClientConfig struct {
	// 服务端地址（ZeroTier IP:端口）
	ServerAddr string `yaml:"server_addr" json:"server_addr"`
	// 服务端 m3u 路径
	ServerM3UPath string `yaml:"server_m3u_path" json:"server_m3u_path"`
	// 本地代理监听地址
	ListenAddr string `yaml:"listen_addr" json:"listen_addr"`
	// 自定义分类（追加在原同步列表之后，地址原样下发，不走本地代理）
	CustomGroups []CustomGroup `yaml:"custom_groups" json:"custom_groups"`
	// 本地 m3u 路径
	M3UPath string `yaml:"m3u_path" json:"m3u_path"`
	// 流代理路径前缀
	StreamPath string `yaml:"stream_path" json:"stream_path"`
	// 隧道连通性检测间隔（秒）
	TunnelCheckInterval int `yaml:"tunnel_check_interval" json:"tunnel_check_interval"`
	// 上游超时（秒）
	UpstreamTimeout int `yaml:"upstream_timeout" json:"upstream_timeout"`
}

// WebConfig WebUI 配置
type WebConfig struct {
	ListenAddr string `yaml:"listen_addr" json:"listen_addr"`
	Username   string `yaml:"username" json:"username"`
	Password   string `yaml:"password" json:"password"`
	SessionKey string `yaml:"session_key" json:"session_key"`
}

// RuntimePaths 运行时路径
type RuntimePaths struct {
	ConfigFile string
	DataDir    string
	LogDir     string
	WebRoot    string
}

// DefaultConfig 返回默认配置（不硬编码任何网络 ID / 密钥）
func DefaultConfig() *Config {
	return &Config{
		Mode:  ModeServer,
		Token: randomHex(16),
		ZeroTier: ZeroTierConfig{
			APIPort:           9993,
			EnableTun:         true,
			FallbackUserspace: true,
		},
		RTP2HTTPD: RTP2HTTPDConfig{
			ListenAddr: "127.0.0.1:8080",
			SourceM3U:  "",
		},
		Server: ServerConfig{
			SourceM3U:           "",
			ListenAddr:          "0.0.0.0:8081",
			M3UPath:             "/live.m3u",
			StreamPath:          "/stream/",
			HealthCheckInterval: 300,
			HealthCheckTimeout:  5,
		},
		Client: ClientConfig{
			ServerAddr:          "",
			ServerM3UPath:       "/live.m3u",
			ListenAddr:          "0.0.0.0:8086",
			M3UPath:             "/live.m3u",
			StreamPath:          "/stream/",
			TunnelCheckInterval: 60,
			UpstreamTimeout:     30,
		},
		Web: WebConfig{
			ListenAddr: ":8089",
			Username:   "admin",
			Password:   "admin",
			SessionKey: randomHex(32),
		},
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("fallback-%d", os.Getpid())
	}
	return hex.EncodeToString(b)
}

// Load 从文件加载配置
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, fmt.Errorf("create config dir: %w", err)
		}
		if err := cfg.Save(path); err != nil {
			return nil, fmt.Errorf("save default config: %w", err)
		}
		cfg.Runtime.ConfigFile = path
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.Runtime.ConfigFile = path
	return cfg, nil
}

// Save 保存配置到文件
func (c *Config) Save(path string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// Get 获取配置的 JSON 副本
func (c *Config) Get() map[string]interface{} {
	c.mu.RLock()
	defer c.mu.RUnlock()

	data, _ := json.Marshal(c)
	var m map[string]interface{}
	_ = json.Unmarshal(data, &m)
	return m
}

// Update 部分更新配置
func (c *Config) Update(patch map[string]interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, c)
}

// GetMode 线程安全读取模式
func (c *Config) GetMode() Mode {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Mode
}

// SetMode 线程安全设置模式
func (c *Config) SetMode(m Mode) {
	c.mu.Lock()
	c.Mode = m
	c.mu.Unlock()
}
