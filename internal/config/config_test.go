package config

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestStaticDatabasePathDefaultAndEnvOverride(t *testing.T) {
	if got := defaults().SQLite.StaticPath; got != "server_data/static.sqlite3" {
		t.Fatalf("默认静态库路径=%q", got)
	}
	if got := defaults().Launcher.GameRegion; got != "CN" {
		t.Fatalf("默认游戏区域=%q", got)
	}
	t.Setenv("RF4_SQLITE_STATIC_PATH", "custom/static.sqlite3")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.SQLite.StaticPath; got != "custom/static.sqlite3" {
		t.Fatalf("环境变量静态库路径=%q", got)
	}
}

func TestAdminPasswordEnvOverride(t *testing.T) {
	t.Setenv("RF4_ADMIN_PASSWORD", "compose-test-password")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin.Password != "compose-test-password" {
		t.Fatal("RF4_ADMIN_PASSWORD did not override the configured password")
	}
}

func TestGameLanguageFollowsRegionUnlessSet(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{"默认 CN", "", "zh"},
		{"EN 区推断英文", "gateway:\n  region: EN\n", "en"},
		{"小写 cn 仍是中文", "gateway:\n  region: cn\n", "zh"},
		{"显式覆盖优先", "gateway:\n  region: EN\ngame:\n  language: ZH\n", "zh"},
		{"CN 区显式英文", "game:\n  language: en\n", "en"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Game.Language != tc.want {
				t.Fatalf("game.language=%q，期望 %q", cfg.Game.Language, tc.want)
			}
		})
	}
}

func TestGameLanguageEnvOverrideAndInvalid(t *testing.T) {
	t.Setenv("RF4_GAME_LANGUAGE", "en")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Game.Language != "en" {
		t.Fatalf("RF4_GAME_LANGUAGE=%q", cfg.Game.Language)
	}
	t.Setenv("RF4_GAME_LANGUAGE", "fr")
	if _, err := Load(""); err == nil {
		t.Fatal("非法语言 fr 应报错")
	}
}

func TestLoginCompatEnvOverride(t *testing.T) {
	t.Setenv("RF4_GATEWAY_LOGIN_COMPAT_ADDR", ":7515")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway.LoginCompatAddr != ":7515" {
		t.Fatalf("RF4_GATEWAY_LOGIN_COMPAT_ADDR=%q", cfg.Gateway.LoginCompatAddr)
	}
}

// TestValidateRejectsInvalidMaxConns 挡住两种会把接入面打死的取值。
//
// max_conns: 0 让 sem 变成无缓冲 channel，第二条连接起 Accept 循环永久阻塞；
// 负数让 make(chan, n) 直接 panic。两者都必须在加载配置时就报错，而不是等到
// 第一条连接卡在握手上。
func TestValidateRejectsInvalidMaxConns(t *testing.T) {
	for _, n := range []int{0, -5} {
		cfg := defaults()
		cfg.Gateway.MaxConns = n
		err := cfg.validate()
		if !errors.Is(err, ErrMaxConns) {
			t.Fatalf("max_conns=%d 应被拒绝，实际 err=%v", n, err)
		}
		if !strings.Contains(err.Error(), "gateway.max_conns") {
			t.Fatalf("错误信息未指出配置项名：%v", err)
		}
		if !strings.Contains(err.Error(), strconv.Itoa(n)) {
			t.Fatalf("错误信息未带上当前值 %d：%v", n, err)
		}
	}
}

// TestValidateMaxConnsBounds 钉住合法区间与边界，同时守住默认值不变。
func TestValidateMaxConnsBounds(t *testing.T) {
	if DefaultMaxConns != 512 {
		t.Fatalf("默认 max_conns 变了：%d", DefaultMaxConns)
	}
	if got := defaults().Gateway.MaxConns; got != DefaultMaxConns {
		t.Fatalf("默认配置 max_conns=%d", got)
	}

	for _, tc := range []struct {
		name string
		n    int
		ok   bool
	}{
		{"下限", 1, true},
		{"默认值", DefaultMaxConns, true},
		{"上限", MaxAllowedConns, true},
		{"上限加一", MaxAllowedConns + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaults()
			cfg.Gateway.MaxConns = tc.n
			err := cfg.validate()
			if tc.ok && err != nil {
				t.Fatalf("max_conns=%d 应通过，实际 err=%v", tc.n, err)
			}
			if !tc.ok && !errors.Is(err, ErrMaxConns) {
				t.Fatalf("max_conns=%d 应被拒绝，实际 err=%v", tc.n, err)
			}
		})
	}
}

// TestLoadRejectsInvalidMaxConns 走完整加载链路（默认值 + 环境变量 + 校验）。
func TestLoadRejectsInvalidMaxConns(t *testing.T) {
	t.Setenv("RF4_GATEWAY_MAX_CONNS", "0")
	if _, err := Load(""); !errors.Is(err, ErrMaxConns) {
		t.Fatalf("RF4_GATEWAY_MAX_CONNS=0 应在加载时报错，实际 err=%v", err)
	}

	t.Setenv("RF4_GATEWAY_MAX_CONNS", "64")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway.MaxConns != 64 {
		t.Fatalf("RF4_GATEWAY_MAX_CONNS 覆盖失败：%d", cfg.Gateway.MaxConns)
	}
}

// 发布包开箱即用：管理后台只监听本机回环，凭据仍为可登录的 admin/admin123。
func TestAdminDefaultsStayOpenOutOfBox(t *testing.T) {
	want := Admin{
		HTTPAddr:  "127.0.0.1:8080",
		JWTSecret: DefaultAdminJWTSecret,
		ObsAddr:   ":9503",
		Username:  DefaultAdminUsername,
		Password:  DefaultAdminPassword,
	}
	if got := defaults().Admin; got != want {
		t.Fatalf("管理后台默认值应为 %+v，实得 %+v", want, got)
	}
	user, pass := defaults().Admin.EffectiveCredentials()
	if user != "admin" || pass != "admin123" {
		t.Fatalf("默认有效凭据应为 admin/admin123，实得 %s/%s", user, pass)
	}
}

// 显式配置必须覆盖默认值，包括把监听地址放回所有网卡。
func TestAdminConfigOverridesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := "admin:" + "\n" +
		"  http_addr: \":8080\"" + "\n" +
		"  jwt_secret: \"lan-secret\"" + "\n" +
		"  username: \"boss\"" + "\n" +
		"  password: \"strong-pass\"" + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("写测试配置失败: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载测试配置失败: %v", err)
	}
	if cfg.Admin.HTTPAddr != ":8080" {
		t.Fatalf("http_addr 应取配置值 :8080，实得 %q", cfg.Admin.HTTPAddr)
	}
	if cfg.Admin.JWTSecret != "lan-secret" {
		t.Fatalf("jwt_secret 应取配置值 lan-secret，实得 %q", cfg.Admin.JWTSecret)
	}
	if user, pass := cfg.Admin.EffectiveCredentials(); user != "boss" || pass != "strong-pass" {
		t.Fatalf("有效凭据应取配置值 boss/strong-pass，实得 %s/%s", user, pass)
	}
	if !cfg.Admin.ListenAllInterfaces() {
		t.Fatalf("显式配置 %q 时应判定为监听所有网卡，实际没有", cfg.Admin.HTTPAddr)
	}
	if got := cfg.Admin.WeakWarnings(); len(got) != 0 {
		t.Fatalf("自定义凭据与密钥不应产生告警，实得 %v", got)
	}
}

// 弱口令/默认密钥必须被识别成告警，自定义值不得误报。
func TestAdminWeakWarningsReportDefaultCredentials(t *testing.T) {
	weak := Admin{Username: "admin", Password: "admin123", JWTSecret: "change-me"}
	got := weak.WeakWarnings()
	if len(got) != 2 {
		t.Fatalf("默认凭据与默认密钥应各出一条告警，实得 %d 条: %v", len(got), got)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"admin.password", "admin123", "admin.jwt_secret"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("弱口令告警应包含 %q 以便照着改，实得: %s", want, joined)
		}
	}

	empty := Admin{Username: "admin", Password: "", JWTSecret: ""}
	if got := empty.WeakWarnings(); len(got) != 2 {
		t.Fatalf("空密码与空密钥应各出一条告警，实得 %d 条: %v", len(got), got)
	}

	strong := Admin{Username: "admin", Password: "strong-pass", JWTSecret: "lan-secret"}
	if got := strong.WeakWarnings(); len(got) != 0 {
		t.Fatalf("自定义凭据不应告警，实得 %v", got)
	}
}

// 用户名为空时 EffectiveCredentials 回退到默认 admin/admin123，告警也必须
// 按这个实际生效值给出；配置里写了别的密码不能掩盖回退后的弱口令。
func TestAdminWeakWarningsUseEffectiveCredentials(t *testing.T) {
	a := Admin{Username: "", Password: "strong-pass", JWTSecret: "lan-secret"}
	user, pass := a.EffectiveCredentials()
	if user != DefaultAdminUsername || pass != DefaultAdminPassword {
		t.Fatalf("空用户名应回退到 %s/%s，实得 %s/%s", DefaultAdminUsername, DefaultAdminPassword, user, pass)
	}
	got := a.WeakWarnings()
	if len(got) != 1 {
		t.Fatalf("回退默认密码应只报一条弱口令告警，实得 %d 条: %v", len(got), got)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"admin.username", DefaultAdminPassword} {
		if !strings.Contains(joined, want) {
			t.Fatalf("告警应包含 %q 以便照着改，实得: %s", want, joined)
		}
	}
}

// 只有回环地址才算安全默认；通配地址与主机名都视为对外开放。
func TestAdminListenAllInterfaces(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8080", false},
		{"localhost:8080", false},
		{"[::1]:8080", false},
		{":8080", true},
		{"0.0.0.0:8080", true},
		{"192.168.1.10:8080", true},
		{"admin.example.com:8080", true},
		{"", true},
	}
	for _, tc := range cases {
		if got := (Admin{HTTPAddr: tc.addr}).ListenAllInterfaces(); got != tc.want {
			t.Fatalf("http_addr=%q 应判定监听所有网卡=%v，实得 %v", tc.addr, tc.want, got)
		}
	}
}
