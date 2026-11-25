package main

import (
	"bytes"
	_ "embed"
	"strconv"

	"rf4server/internal/admin"
	"rf4server/internal/config"
)

// panel_i18n.js 是控制面板的英文翻译层（specs/changes/ui-english-i18n 阶段 2）。
// 服务 panel.html 时注入，不改 panel.html 本身，上游对面板的改动可以直接合入。
//
//go:embed panel_i18n.js
var panelI18nJS []byte

// panelPage 返回注入了翻译脚本的面板页面；lang 为默认语言（en 时默认英文，
// 其他按中文），页面上的 EN/中文 切换按钮可覆盖并记在浏览器里。
func panelPage(html []byte, lang string) []byte {
	def := config.LanguageZH
	if lang == config.LanguageEN {
		def = config.LanguageEN
	}
	var inject bytes.Buffer
	inject.WriteString("<script>window.RF4_DEFAULT_LANG=")
	inject.WriteString(strconv.Quote(def))
	inject.WriteString(";</script>\n<script>\n")
	inject.Write(admin.I18nEngineJS()) // 与管理后台共用同一份翻译引擎
	inject.WriteString("\n</script>\n<script>\n")
	inject.Write(panelI18nJS)
	inject.WriteString("\n</script>\n")

	at := bytes.LastIndex(bytes.ToLower(html), []byte("</body>"))
	if at < 0 {
		return append(append([]byte{}, html...), inject.Bytes()...)
	}
	out := make([]byte, 0, len(html)+inject.Len())
	out = append(out, html[:at]...)
	out = append(out, inject.Bytes()...)
	return append(out, html[at:]...)
}
