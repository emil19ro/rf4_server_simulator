package admin

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rf4server/internal/config"
	"rf4server/internal/logging"
)

func namesFromScript(t *testing.T, body string) map[string]string {
	t.Helper()
	const prefix = "window.RF4I18N_NAMES="
	if !strings.HasPrefix(body, prefix) {
		t.Fatalf("names_en.js 格式异常：%.80q", body)
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(body, prefix)), ";")), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestNamesScriptMapsDatabaseChineseNamesToClientEnglish(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	srv := NewServer(config.Admin{JWTSecret: "test"}, testDB(t), logging.Discard(), WebFS())

	// 没有 ref/client_names.json：空字典，页面照常。
	if got := namesFromScript(t, getAdminPath(t, srv, "/i18n/names_en.js").Body.String()); len(got) != 0 {
		t.Fatalf("缺少导出文件时应为空字典：%v", got)
	}

	for _, q := range []string{
		`INSERT OR REPLACE INTO item_catalog(item_id,name,name_zh) VALUES('names_test_worm','蚯蚓测试','蚯蚓测试')`,
		`INSERT OR REPLACE INTO item_catalog(item_id,name,name_zh) VALUES('names_test_rod','Corona S60MH','Corona S60MH')`,
		`INSERT INTO loc_wiki_fish(loc,fish_key,fish_zh) VALUES('names-test','names_test_fish','测试鲈鱼` + bomRune + `')`,
	} {
		if _, err := srv.db.SQL.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "ref"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := `{"version":1,"names":{"names_test_worm":"Test Worm","names_test_rod":"Corona S60MH","names_test_fish":"Test Perch"},` +
		`"zh2en":{"毛翅目幼虫测试":"Test Caddisfly","不翻译":"仍然中文"}}`
	if err := os.WriteFile(filepath.Join(dir, "ref", clientNamesFile), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := getAdminPath(t, srv, "/i18n/names_en.js")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("names_en.js = %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	got := namesFromScript(t, rec.Body.String())
	for zh, want := range map[string]string{
		"蚯蚓测试":           "Test Worm",      // 物品：item_id → 客户端英文名
		"测试鲈鱼":           "Test Perch",     // 鱼：fish_key，去掉 BOM 的键
		"测试鲈鱼" + bomRune: "Test Perch",     // 原样带 BOM 的键也有
		"毛翅目幼虫测试":        "Test Caddisfly", // 客户端 zh2en 词条
	} {
		if got[zh] != want {
			t.Fatalf("%q → %q，期望 %q（全部：%v）", zh, got[zh], want, got)
		}
	}
	if _, ok := got["Corona S60MH"]; ok {
		t.Fatal("非中文名称不应进字典")
	}
	if _, ok := got["不翻译"]; ok {
		t.Fatal("英文侧仍是中文的词条不应进字典")
	}
}
