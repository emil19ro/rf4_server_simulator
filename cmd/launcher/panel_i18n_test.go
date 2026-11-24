package main

import (
	"bytes"
	"strings"
	"testing"
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
	page := panelPage([]byte("<p>x</p>"), "en")
	if !bytes.HasPrefix(page, []byte("<p>x</p>")) || !bytes.Contains(page, []byte("RF4_DEFAULT_LANG")) {
		t.Fatalf("没有 </body> 时应追加到末尾：%q", page)
	}
}

func TestPanelHTMLStaysUntouchedByInjection(t *testing.T) {
	orig := append([]byte{}, panelHTML...)
	_ = panelPage(panelHTML, "en")
	if !bytes.Equal(orig, panelHTML) {
		t.Fatal("注入不得修改内嵌的 panelHTML")
	}
}
