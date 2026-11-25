package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strconv"

	"rf4server/internal/config"
)

// 英文界面（specs/changes/ui-english-i18n 阶段 3）：服务 index.html 时注入
// web/i18n/engine.js 与 web/i18n/admin_dict.js，前端源文件不改，上游后台改动
// 可直接合入；字典里没有的新文案保持中文。

// I18nEngineJS 返回共享的界面翻译引擎（launcher 控制面板也注入同一份）。
func I18nEngineJS() []byte {
	b, err := webRoot.ReadFile("web/i18n/engine.js")
	if err != nil {
		return nil
	}
	return b
}

// SetLanguage 设置后台默认界面语言（game.language）；en 以外按中文。
// 页面上的 EN/中文 按钮可覆盖，并记在浏览器里（与控制面板共用）。
func (s *Server) SetLanguage(lang string) {
	if lang == config.LanguageEN {
		s.lang = config.LanguageEN
		return
	}
	s.lang = config.LanguageZH
}

// i18nVersion 按引擎与字典内容生成的短哈希，拼进脚本地址：升级后字典变了，
// 浏览器不会继续用缓存里的旧字典。
var i18nVersion = func() string {
	h := sha256.New()
	for _, name := range []string{"web/i18n/engine.js", "web/i18n/admin_dict.js"} {
		b, _ := webRoot.ReadFile(name)
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}()

// i18nScripts 是注入到 </body> 前的脚本：默认语言 + 引擎 + 后台字典。
func i18nScripts(lang string) []byte {
	if lang != config.LanguageEN {
		lang = config.LanguageZH
	}
	return []byte("<script>window.RF4_DEFAULT_LANG=" + strconv.Quote(lang) + ";</script>\n" +
		`<script src="i18n/engine.js?v=` + i18nVersion + `"></script>` + "\n" +
		`<script src="i18n/admin_dict.js?v=` + i18nVersion + `"></script>` + "\n")
}

// injectBeforeBody 把 extra 插到最后一个 </body> 之前；没有 </body> 时追加。
func injectBeforeBody(html, extra []byte) []byte {
	at := bytes.LastIndex(bytes.ToLower(html), []byte("</body>"))
	if at < 0 {
		return append(append([]byte{}, html...), extra...)
	}
	out := make([]byte, 0, len(html)+len(extra))
	out = append(out, html[:at]...)
	out = append(out, extra...)
	return append(out, html[at:]...)
}

// webHandler 静态前端：入口页注入翻译脚本，其余文件原样走 FileServer。
func (s *Server) webHandler(webFS fs.FS) http.Handler {
	files := http.FileServer(http.FS(webFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "" {
			files.ServeHTTP(w, r)
			return
		}
		index, err := fs.ReadFile(webFS, "index.html")
		if err != nil {
			files.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(injectBeforeBody(index, i18nScripts(s.lang)))
	})
}
