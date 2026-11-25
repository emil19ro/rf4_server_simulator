package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rf4server/internal/config"
	"rf4server/internal/logging"
)

func TestAboutReadsLocalREADME(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# 本地 README\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(config.Admin{JWTSecret: "test"}, testDB(t), logging.Discard(), nil)
	rec := httptest.NewRecorder()
	srv.handleAbout(rec, httptest.NewRequest(http.MethodGet, "/api/about", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("关于页状态 = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Content, "# 本地 README") {
		t.Fatalf("应优先读取本地 README: %q", got.Content)
	}
}

func TestAboutEnglishPrefersREADMEEn(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	about := func(srv *Server) string {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleAbout(rec, httptest.NewRequest(http.MethodGet, "/api/about", nil))
		var got struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got.Content
	}
	srv := NewServer(config.Admin{JWTSecret: "test"}, testDB(t), logging.Discard(), nil)
	srv.SetLanguage("en")
	// 没有本地文件时用内嵌的 README.en.md（英文 README 的标题行）。
	if got := about(srv); !strings.Contains(got, "## Quick start") || strings.Contains(got, "## 快速开始") {
		t.Fatalf("英文界面应返回内嵌 README.en.md: %.80q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# 中文\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.en.md"), []byte("# Local English\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := about(srv); !strings.Contains(got, "# Local English") {
		t.Fatalf("英文界面应优先读本地 README.en.md: %q", got)
	}
	srv.SetLanguage("zh")
	if got := about(srv); !strings.Contains(got, "# 中文") {
		t.Fatalf("中文界面仍读 README.md: %q", got)
	}
}
