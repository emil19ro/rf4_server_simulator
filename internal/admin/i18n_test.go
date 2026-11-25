package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rf4server/internal/config"
	"rf4server/internal/logging"
)

func getAdminPath(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestAdminIndexInjectsTranslatorBeforeBody(t *testing.T) {
	srv := NewServer(config.Admin{JWTSecret: "test"}, testDB(t), logging.Discard(), WebFS())
	srv.SetLanguage("en")
	rec := getAdminPath(t, srv, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	body := rec.Body.String()
	lang := strings.Index(body, `<script>window.RF4_DEFAULT_LANG="en";</script>`)
	engine := strings.Index(body, `<script src="i18n/engine.js?v=`+i18nVersion+`"></script>`)
	dict := strings.Index(body, `<script src="i18n/admin_dict.js?v=`+i18nVersion+`"></script>`)
	if len(i18nVersion) != 12 {
		t.Fatalf("i18nVersion=%q，应为 12 位内容哈希", i18nVersion)
	}
	end := strings.LastIndex(body, "</body>")
	if lang < 0 || engine < lang || dict < engine || end < dict {
		t.Fatalf("注入顺序应为 默认语言 → 引擎 → 字典 → </body>：%q", body)
	}
	if !strings.Contains(body, `<script type="module" src="app/main.js"></script>`) {
		t.Fatal("原有入口脚本必须保留")
	}
	if !strings.Contains(body, `localStorage.getItem("rf4.theme.fork")`) {
		t.Fatal("首访自动写入的 system 也要切到深色一次（rf4.theme.fork 标记）")
	}
	boot := strings.Index(body, `if(!v){v="dark";`)
	if boot < 0 || boot > strings.Index(body, "</head>") {
		t.Fatal("主题启动脚本应在 </head> 之前，未设置时默认深色")
	}
	if strings.Contains(body, `prefers-color-scheme`) {
		t.Fatal("管理后台的「跟随系统」由 light-dark() 处理，不应解析成固定值")
	}
}

func TestAdminDefaultLanguageIsChinese(t *testing.T) {
	srv := NewServer(config.Admin{JWTSecret: "test"}, testDB(t), logging.Discard(), WebFS())
	if body := getAdminPath(t, srv, "/").Body.String(); !strings.Contains(body, `window.RF4_DEFAULT_LANG="zh"`) {
		t.Fatal("未调用 SetLanguage 时默认中文")
	}
	srv.SetLanguage("fr")
	if body := getAdminPath(t, srv, "/").Body.String(); !strings.Contains(body, `window.RF4_DEFAULT_LANG="zh"`) {
		t.Fatal("非 en 语言按中文")
	}
}

func TestAdminServesI18nScripts(t *testing.T) {
	srv := NewServer(config.Admin{JWTSecret: "test"}, testDB(t), logging.Discard(), WebFS())
	if rec := getAdminPath(t, srv, "/i18n/engine.js"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "RF4I18N") {
		t.Fatalf("engine.js = %d", rec.Code)
	}
	if rec := getAdminPath(t, srv, "/i18n/admin_dict.js"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"玩家管理": "Players"`) {
		t.Fatalf("admin_dict.js = %d", rec.Code)
	}
	if !strings.Contains(string(I18nEngineJS()), "makeTranslator") {
		t.Fatal("I18nEngineJS 应返回内嵌的引擎源码")
	}
}
