package main

import (
	"bytes"
	"strings"
	"testing"

	"rf4server/internal/config"
)

func TestPanelPageInjectsTranslatorBeforeBody(t *testing.T) {
	page := string(panelPage([]byte("<html><body><p>首页</p></BODY></html>"), "en"))
	at := strings.Index(page, `<script>window.RF4_DEFAULT_LANG="en";</script>`)
	end := strings.Index(page, "</BODY>")
	if at < 0 || end < 0 || at > end {
		t.Fatalf("翻译脚本必须注入到 </body> 之前：%q", page)
	}
	if !strings.Contains(page, `"首页": "Home"`) || !strings.Contains(page, "rf4LangToggle") {
		t.Fatal("页面缺少翻译字典或语言切换按钮")
	}
}

func TestPanelPageDefaultLanguage(t *testing.T) {
	for lang, want := range map[string]string{
		"en": `window.RF4_DEFAULT_LANG="en"`,
		"zh": `window.RF4_DEFAULT_LANG="zh"`,
		"":   `window.RF4_DEFAULT_LANG="zh"`,
		"fr": `window.RF4_DEFAULT_LANG="zh"`,
	} {
		if page := string(panelPage([]byte("<body></body>"), lang)); !strings.Contains(page, want) {
			t.Fatalf("lang=%q 时应包含 %s", lang, want)
		}
	}
}

func TestPanelPageWithoutBodyAppends(t *testing.T) {
	// 没有 </head> 时主题脚本放最前面（首帧前生效），没有 </body> 时翻译脚本追加到末尾。
	page := panelPage([]byte("<p>x</p>"), "en")
	content := bytes.Index(page, []byte("<p>x</p>"))
	theme := bytes.Index(page, []byte(`localStorage.getItem(k)`))
	tr := bytes.Index(page, []byte("RF4_DEFAULT_LANG"))
	if theme < 0 || content < theme || tr < content {
		t.Fatalf("应为 主题脚本 → 原内容 → 翻译脚本：theme=%d content=%d tr=%d", theme, content, tr)
	}
}

func TestPanelPageInjectsDarkThemeInHead(t *testing.T) {
	page := string(panelPage([]byte("<html><head><style>.a{}</style></head><body></body></html>"), "en"))
	boot := strings.Index(page, `localStorage.getItem(k)`)
	dark := strings.Index(page, `:root[data-theme=dark]{`)
	orig := strings.Index(page, `<style>.a{}</style>`)
	head := strings.Index(page, "</head>")
	if boot < 0 || dark < 0 || orig > boot || dark > head {
		t.Fatalf("主题脚本与深色样式应注入在原有样式之后、</head> 之前：%q", page)
	}
	if !strings.Contains(page, `if(!v){v="dark";`) {
		t.Fatal("未设置主题时应默认深色")
	}
	if !strings.Contains(page, `prefers-color-scheme: dark`) {
		t.Fatal("控制面板应把「跟随系统」解析成 light/dark")
	}
}

func TestTrayTextFollowsLanguage(t *testing.T) {
	if got := (&app{}).tr("退出", "Exit"); got != "退出" {
		t.Fatalf("未加载配置时应为中文：%q", got)
	}
	a := &app{cfg: &config.Config{Game: config.Game{Language: "en"}}}
	if got := a.tr("退出", "Exit"); got != "Exit" {
		t.Fatalf("language=en 时托盘应为英文：%q", got)
	}
}

func TestPanelHTMLStaysUntouchedByInjection(t *testing.T) {
	orig := append([]byte{}, panelHTML...)
	_ = panelPage(panelHTML, "en")
	if !bytes.Equal(orig, panelHTML) {
		t.Fatal("注入不得修改内嵌的 panelHTML")
	}
}
