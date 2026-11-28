// Package config 提供 YAML 配置加载和环境变量覆盖。
//
// 空配置使用开发默认值；敏感配置通过 config.yaml 或 RF4_ 环境变量注入，
// 仓库只保留示例配置。
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 全量配置。每个进程只读自己关心的部分。
type Config struct {
	Env      string   `yaml:"env"` // dev | prod
	Log      Log      `yaml:"log"`
	SQLite   SQLite   `yaml:"sqlite"`
	Gateway  Gateway  `yaml:"gateway"`
	Game     Game     `yaml:"game"`
	Admin    Admin    `yaml:"admin"`
	Launcher Launcher `yaml:"launcher"`
}

// SQLite 本地单文件数据库（多进程共享，WAL 并发）。
type SQLite struct {
	Path       string `yaml:"path"`        // 主库，例如 data/rf4server.db
	StaticPath string `yaml:"static_path"` // 只读静态配置库，例如 server_data/static.sqlite3
}

type Log struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // text | json
}

// Tunnel 网关↔主服内部隧道（gameserver 监听、gateway 拨号）。
type Tunnel struct {
	Addr string `yaml:"addr"` // gameserver 内部监听地址，如 127.0.0.1:9401
	PSK  string `yaml:"psk"`  // 预共享密钥，隧道握手认证
}

type Gateway struct {
	LoginAddr             string  `yaml:"login_addr"`        // HTTPS 登录监听，如 :443
	LoginCompatAddr       string  `yaml:"login_compat_addr"` // 可选：旧版 CN 客户端的明文 HTTP 登录兼容口，如 127.0.0.1:7515
	RealtimeAddr          string  `yaml:"realtime_addr"`     // realtime TCP 监听，如 :9341
	CertFile              string  `yaml:"cert_file"`
	KeyFile               string  `yaml:"key_file"`
	AdvertiseHost         string  `yaml:"advertise_host"`          // 写入 <logon><host> 的地址
	AdvertisePort         int     `yaml:"advertise_port"`          // 写入 <logon><port>
	Region                string  `yaml:"region"`                  // CN → 客户端中文界面
	ServerName            string  `yaml:"server_name"`             // 写入 <logon><server>
	LoginDomain           string  `yaml:"login_domain"`            // api.rf4game.ru（供日志/工具用）
	MaxConns              int     `yaml:"max_conns"`               // realtime 最大并发连接
	AckClientFrames       bool    `yaml:"ack_client_frames"`       // 先 ACK 后转发（协议要求，默认开启）
	TransportKeepaliveSec float64 `yaml:"transport_keepalive_sec"` // 4 字节零帧保活间隔

	Tunnel Tunnel `yaml:"tunnel"`

	ObsAddr string `yaml:"obs_addr"` // 观测端口 /healthz /readyz /metrics /debug/pprof

	TokenTTL          int     `yaml:"token_ttl_days"`         // 登录 token 有效期（天）
	UseridStart       int64   `yaml:"userid_start"`           // 自增 userid 起点
	BiteProbability   float64 `yaml:"bite_probability"`       // 起竿判出鱼概率（1.0=每杆必出）
	BiteDelayScale    float64 `yaml:"bite_delay_scale"`       // 咬钩延迟倍率
	BiteDelayFixed    float64 `yaml:"bite_delay_fixed"`       // 固定咬钩延迟秒（>0 时忽略分布）
	KeepaliveInterval float64 `yaml:"keepalive_interval_sec"` // 2/2 世界时钟推送间隔（秒）
}

type Game struct {
	Tunnel        Tunnel  `yaml:"tunnel"` // 与 Gateway.Tunnel 相同配置
	ObsAddr       string  `yaml:"obs_addr"`
	DefaultUserid int64   `yaml:"default_userid"` // 单账号模式下使用的固定 userid
	Weather       Weather `yaml:"weather"`

	KeepaliveIntervalSec float64 `yaml:"keepalive_interval_sec"` // 2/2 世界时钟推送间隔（秒）

	// Language 服务端拼装的聊天播报与鱼名语言：zh | en；留空按 gateway.region 推断。
	Language string `yaml:"language"`
}

// 服务端文案语言取值。
const (
	LanguageZH = "zh"
	LanguageEN = "en"
)

// Weather 控制进程启动时生成的逐日天气 kind 序列。
// 序列在本进程内固定，每次启动重新随机；关闭时沿用地图模板的固定顺序。
type Weather struct {
	Randomize bool `yaml:"randomize"`  // true=启动时按图加权生成天气 kind
	CycleDays int  `yaml:"cycle_days"` // 每图序列长度，最小 8，默认 64
}

// KeepaliveInterval 2/2 保活间隔；必须小于客户端断开阈值（~10s）。
func (g *Game) KeepaliveInterval() time.Duration {
	if g.KeepaliveIntervalSec <= 0 {
		return 5 * time.Second
	}
	return time.Duration(g.KeepaliveIntervalSec * float64(time.Second))
}

type Admin struct {
	HTTPAddr  string `yaml:"http_addr"` // 仅 cmd/admin 独立监听；默认只绑回环，需要局域网访问时显式改成 :8080
	JWTSecret string `yaml:"jwt_secret"`
	ObsAddr   string `yaml:"obs_addr"`
	Username  string `yaml:"username"` // 管理员账号（以配置为准，启动时同步）
	Password  string `yaml:"password"` // 管理员密码（明文进 yaml，注意文件权限）
}

// 本地开箱默认值：发布包首次启动即可登录管理后台，对外开放前必须替换。
const (
	DefaultAdminUsername  = "admin"
	DefaultAdminPassword  = "admin123"
	DefaultAdminJWTSecret = "change-me"
)

// EffectiveCredentials 返回启动时应同步到 admin_user 的账号与密码。
// 用户名缺省时整体回退到本地默认 admin/admin123，与发布包开箱即用的约定一致；
// 只配置了用户名而没有配置密码时保持空密码（由 WeakWarnings 提示），不擅自套用默认值。
func (a Admin) EffectiveCredentials() (username, password string) {
	if strings.TrimSpace(a.Username) == "" {
		return DefaultAdminUsername, DefaultAdminPassword
	}
	return a.Username, a.Password
}

// ListenAllInterfaces 报告 http_addr 是否会监听非回环网卡。
// 空地址、":8080" 形式的通配地址与主机名都算；只有回环地址和 localhost 不算。
func (a Admin) ListenAllInterfaces() bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(a.HTTPAddr))
	if err != nil {
		host = strings.TrimSpace(a.HTTPAddr)
	}
	if host == "" {
		return true
	}
	if strings.EqualFold(host, "localhost") {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

// WeakWarnings 列出管理端仍在用默认凭据或默认签名密钥的项，供各进程启动时告警。
// 返回空切片表示没有发现问题；文案直接给出配置项名，便于照着改。
func (a Admin) WeakWarnings() []string {
	// 告警必须按实际会写入 admin_user 的凭据判断：用户名为空时
	// EffectiveCredentials 会整体回退到 admin/admin123，此时即使配置里写了
	// 自定义密码，实际生效的仍是弱口令。
	username, password := a.EffectiveCredentials()
	out := []string{}
	switch {
	case strings.TrimSpace(password) == "":
		out = append(out, "管理后台密码为空：请设置 config.yaml 的 admin.password 或环境变量 RF4_ADMIN_PASSWORD")
	case password == DefaultAdminPassword:
		if strings.TrimSpace(a.Username) == "" {
			out = append(out, "管理后台用户名 "+username+" 来自缺省回退，实际使用默认密码 "+DefaultAdminPassword+"：请设置 config.yaml 的 admin.username 和 admin.password，开放到局域网前必须替换")
		} else {
			out = append(out, "管理后台仍使用默认密码 "+DefaultAdminPassword+"：请修改 config.yaml 的 admin.password 或环境变量 RF4_ADMIN_PASSWORD，开放到局域网前必须替换")
		}
	}
	if strings.TrimSpace(a.JWTSecret) == "" || a.JWTSecret == DefaultAdminJWTSecret {
		out = append(out, "管理后台 JWT 签名密钥仍为默认值：请修改 config.yaml 的 admin.jwt_secret 或环境变量 RF4_ADMIN_JWT_SECRET，否则知道默认值的人可以自行签发管理令牌")
	}
	return out
}

// Launcher 本地启动器设置。
type Launcher struct {
	GamePath    string `yaml:"game_path"`    // rf4_x64.exe 绝对路径
	GameRegion  string `yaml:"game_region"`  // 启动参数 -region 的值
	HostsSwitch bool   `yaml:"hosts_switch"` // 开启后：服务启动成功自动把登录域名指向 127.0.0.1，launcher 退出时还原（需管理员权限运行 launcher）
}

// Load 从 path 读取 YAML 并应用环境变量覆盖；path 为空时只用默认值+环境变量。
func Load(path string) (*Config, error) {
	cfg := defaults()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取配置 %s: %w", path, err)
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("解析配置 %s: %w", path, err)
		}
	}
	cfg.applyEnvOverrides()
	cfg.resolveLanguage()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func defaults() *Config {
	return &Config{
		Env:      "dev",
		Log:      Log{Level: "info", Format: "text"},
		Launcher: Launcher{GameRegion: "CN"},
		SQLite: SQLite{
			Path:       "data/rf4server.db",
			StaticPath: "server_data/static.sqlite3",
		},
		Gateway: Gateway{
			LoginAddr:             ":443",
			RealtimeAddr:          ":9341",
			CertFile:              "certs/server.crt",
			KeyFile:               "certs/server.key",
			AdvertiseHost:         "127.0.0.1",
			AdvertisePort:         9341,
			Region:                "CN",
			ServerName:            "RF4_566",
			LoginDomain:           "api.rf4game.ru",
			MaxConns:              DefaultMaxConns,
			AckClientFrames:       true,
			TransportKeepaliveSec: 5.0,
			Tunnel:                Tunnel{Addr: "127.0.0.1:9401", PSK: "change-me"},
			ObsAddr:               ":9501",
			TokenTTL:              365,
			UseridStart:           1000001,
			BiteProbability:       1.0,
			BiteDelayScale:        1.0,
			BiteDelayFixed:        0,
			KeepaliveInterval:     5.0,
		},
		Game: Game{
			Tunnel:               Tunnel{Addr: "127.0.0.1:9401", PSK: "change-me"},
			ObsAddr:              ":9502",
			DefaultUserid:        1000001,
			Weather:              Weather{Randomize: true, CycleDays: 64},
			KeepaliveIntervalSec: 5.0,
		},
		Admin: Admin{
			HTTPAddr:  "127.0.0.1:8080",
			JWTSecret: DefaultAdminJWTSecret,
			ObsAddr:   ":9503",
			Username:  DefaultAdminUsername,
			Password:  DefaultAdminPassword,
		},
	}
}

// applyEnvOverrides 环境变量覆盖：只覆盖部署时最常注入的字段。
func (c *Config) applyEnvOverrides() {
	envStr := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
	envFloat := func(key string, dst *float64) {
		if v, ok := os.LookupEnv(key); ok {
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				*dst = f
			}
		}
	}
	envInt := func(key string, dst *int) {
		if v, ok := os.LookupEnv(key); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				*dst = n
			}
		}
	}
	envBool := func(key string, dst *bool) {
		if v, ok := os.LookupEnv(key); ok {
			if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
				*dst = b
			}
		}
	}

	envStr("RF4_LOG_LEVEL", &c.Log.Level)
	envStr("RF4_LOG_FORMAT", &c.Log.Format)
	envStr("RF4_SQLITE_PATH", &c.SQLite.Path)
	envStr("RF4_SQLITE_STATIC_PATH", &c.SQLite.StaticPath)
	envStr("RF4_GATEWAY_LOGIN_ADDR", &c.Gateway.LoginAddr)
	envStr("RF4_GATEWAY_LOGIN_COMPAT_ADDR", &c.Gateway.LoginCompatAddr)
	envStr("RF4_GATEWAY_REALTIME_ADDR", &c.Gateway.RealtimeAddr)
	envStr("RF4_GATEWAY_ADVERTISE_HOST", &c.Gateway.AdvertiseHost)
	envStr("RF4_GATEWAY_CERT", &c.Gateway.CertFile)
	envStr("RF4_GATEWAY_KEY", &c.Gateway.KeyFile)
	envStr("RF4_TUNNEL_ADDR", &c.Gateway.Tunnel.Addr)
	envStr("RF4_TUNNEL_PSK", &c.Gateway.Tunnel.PSK)
	envStr("RF4_GAME_TUNNEL_PSK", &c.Game.Tunnel.PSK)
	envStr("RF4_ADMIN_HTTP_ADDR", &c.Admin.HTTPAddr)
	envStr("RF4_ADMIN_JWT_SECRET", &c.Admin.JWTSecret)
	envStr("RF4_ADMIN_PASSWORD", &c.Admin.Password)
	envStr("RF4_GAME_CLIENT_PATH", &c.Launcher.GamePath)
	envStr("RF4_GAME_CLIENT_REGION", &c.Launcher.GameRegion)
	envStr("RF4_GAME_LANGUAGE", &c.Game.Language)
	envFloat("RF4_GATEWAY_KEEPALIVE_SEC", &c.Gateway.TransportKeepaliveSec)
	envFloat("RF4_KEEPALIVE_INTERVAL_SEC", &c.Gateway.KeepaliveInterval)
	envFloat("RF4_BITE_PROBABILITY", &c.Gateway.BiteProbability)
	envInt("RF4_GATEWAY_MAX_CONNS", &c.Gateway.MaxConns)
	envBool("RF4_WEATHER_RANDOMIZE", &c.Game.Weather.Randomize)
	envInt("RF4_WEATHER_CYCLE_DAYS", &c.Game.Weather.CycleDays)
}

// resolveLanguage 规范化 game.language；留空时按 gateway.region 推断：
// CN 或未配置 → zh（保持旧行为），其他区域 → en。
func (c *Config) resolveLanguage() {
	lang := strings.ToLower(strings.TrimSpace(c.Game.Language))
	if lang == "" {
		region := strings.ToUpper(strings.TrimSpace(c.Gateway.Region))
		if region == "" || region == "CN" {
			lang = LanguageZH
		} else {
			lang = LanguageEN
		}
	}
	c.Game.Language = lang
}

// realtime 连接上限。默认值与上限都只是容量约束：0 会让连接信号量变成无缓冲
// channel，负数会让 make(chan, n) 直接 panic，所以两者都必须在启动前挡掉。
const (
	// DefaultMaxConns 是 gateway.max_conns 的默认值，也是未配置时的生效值。
	DefaultMaxConns = 512
	// MaxAllowedConns 是 gateway.max_conns 允许的上限。
	MaxAllowedConns = 100000
)

// ErrMaxConns 表示 gateway.max_conns 超出允许范围，调用方可用 errors.Is 判断。
var ErrMaxConns = errors.New("gateway.max_conns 超出允许范围")

func (c *Config) validate() error {
	if c.SQLite.Path == "" {
		return fmt.Errorf("sqlite.path 未配置")
	}
	if c.Game.Language != LanguageZH && c.Game.Language != LanguageEN {
		return fmt.Errorf("game.language 只能是 zh 或 en，当前为 %q", c.Game.Language)
	}
	if c.Gateway.AdvertisePort == 0 {
		return fmt.Errorf("gateway.advertise_port 未配置")
	}
	if err := CheckMaxConns(c.Gateway.MaxConns); err != nil {
		return err
	}
	return nil
}

// CheckMaxConns 校验 realtime 最大并发连接数。
//
// 只有正容量才能约束接入循环：0 会让第二条连接起在 sem 上永久阻塞，负数会让
// make(chan, n) 直接 panic。错误信息带上配置项名与当前值，便于定位配置文件。
func CheckMaxConns(n int) error {
	if n < 1 || n > MaxAllowedConns {
		return fmt.Errorf("%w: gateway.max_conns=%d，允许范围 1..%d", ErrMaxConns, n, MaxAllowedConns)
	}
	return nil
}
