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

func TestReleaseNotesReadsPackageFile(t *testing.T) {
	srv := NewServer(config.Admin{JWTSecret: "test"}, testDB(t), logging.Discard(), nil)
	dir := t.TempDir()
	srv.releaseNotesPath = filepath.Join(dir, releaseNotesFilename)
	if err := os.WriteFile(srv.releaseNotesPath, []byte("# 发布说明\n\n- 本地内容"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/release-notes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("匿名读取发布说明状态 = %d, 期望 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.handleReleaseNotes(rec, httptest.NewRequest(http.MethodGet, "/api/release-notes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("读取发布说明状态 = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Filename string `json:"filename"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Filename != releaseNotesFilename || !strings.Contains(got.Content, "本地内容") {
		t.Fatalf("发布说明内容异常: %+v", got)
	}
}

func TestReleaseNotesFindsEnglishThenLegacyName(t *testing.T) {
	srv := NewServer(config.Admin{JWTSecret: "test"}, testDB(t), logging.Discard(), nil)
	dir := filepath.Join(t.TempDir(), "pkg")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	read := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleReleaseNotes(rec, httptest.NewRequest(http.MethodGet, "/api/release-notes", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("读取发布说明状态 = %d: %s", rec.Code, rec.Body.String())
		}
		var got struct {
			Filename string `json:"filename"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got.Filename
	}
	if err := os.WriteFile(filepath.Join(dir, "发布说明.md"), []byte("# old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "发布说明.md" {
		t.Fatalf("只有旧包文件时 filename=%q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "Release-Notes.md"), []byte("# new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "Release-Notes.md" {
		t.Fatalf("新旧都有时应优先英文文件名，filename=%q", got)
	}
}

func TestReleaseNotesMissingFile(t *testing.T) {
	srv := NewServer(config.Admin{JWTSecret: "test"}, testDB(t), logging.Discard(), nil)
	srv.releaseNotesPath = filepath.Join(t.TempDir(), releaseNotesFilename)
	rec := httptest.NewRecorder()
	srv.handleReleaseNotes(rec, httptest.NewRequest(http.MethodGet, "/api/release-notes", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("缺失发布说明状态 = %d, 期望 404", rec.Code)
	}
}
