package admin

import "bytes"

// 本 fork 默认深色主题：启动脚本在 <head> 里、首帧绘制前读取 localStorage
// "rf4.theme"（与后台主题菜单同一个键），未设置时写入 dark。控制面板与管理
// 后台同源，两边主题保持一致。前端源文件不改。

// ThemeBootScript 返回注入到 </head> 前的主题启动脚本。resolveSystem=true 时
// 把「跟随系统」按系统配色解析成 light/dark（控制面板只认 data-theme）；
// 管理后台自己用 light-dark() 处理「跟随系统」，传 false 保持不设 data-theme。
func ThemeBootScript(resolveSystem bool) []byte {
	system := `return;`
	if resolveSystem {
		system = `v=matchMedia("(prefers-color-scheme: dark)").matches?"dark":"light";`
	}
	// 后台首访会自动把 "system" 写进 rf4.theme，所以不能只在"未设置"时给默认值：
	// 用独立标记 rf4.theme.fork 只切一次深色，之后完全尊重用户在菜单里的选择。
	return []byte(`<script>(function(){try{var k="rf4.theme",v=localStorage.getItem(k);` +
		`if(!localStorage.getItem("rf4.theme.fork")){localStorage.setItem("rf4.theme.fork","1");if(!v||v==="system"){v="dark";localStorage.setItem(k,v);}}` +
		`if(!v){v="dark";localStorage.setItem(k,v);}` +
		`if(v==="system"){` + system + `}` +
		`document.documentElement.setAttribute("data-theme",v);}catch(e){}})();</script>` + "\n")
}

// injectBeforeHead 把 extra 插到第一个 </head> 之前；没有 </head> 时放到最前面。
func injectBeforeHead(html, extra []byte) []byte {
	at := bytes.Index(bytes.ToLower(html), []byte("</head>"))
	if at < 0 {
		return append(append([]byte{}, extra...), html...)
	}
	out := make([]byte, 0, len(html)+len(extra))
	out = append(out, html[:at]...)
	out = append(out, extra...)
	return append(out, html[at:]...)
}

// InjectBeforeHead 供控制面板复用同一个注入逻辑。
func InjectBeforeHead(html, extra []byte) []byte { return injectBeforeHead(html, extra) }
