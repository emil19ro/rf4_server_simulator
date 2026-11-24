package config

import (
	"os"
	"path/filepath"
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
