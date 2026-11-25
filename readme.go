// Package rf4server exposes repository-level assets that are compiled into the binaries.
package rf4server

import _ "embed"

// README 是仓库根目录 README.md 的编译期内嵌副本。
//
//go:embed README.md
var README string

// READMEEn 是 README.en.md 的编译期内嵌副本（英文界面的「关于」页使用）。
//
//go:embed README.en.md
var READMEEn string
