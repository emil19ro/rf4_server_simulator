package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 数据库名称的英文显示层（specs/changes/ui-english-i18n 阶段 4）。
//
// 静态库与玩家库里的物品名、鱼名、饵名很多只有中文。英文名取自客户端自带的
// 本地化（tools/extract_client_names.py 导出到 ref/client_names.json；官方客户端
// 内容不入仓库，同 ref/icons.zip）。这里把「中文名 → 英文名」做成一份字典，
// 以 i18n/names_en.js 下发，前端翻译引擎把它当作额外词条使用。数据库一行不改。

const clientNamesFile = "client_names.json"

// maxClientNamesBytes 限制 ref/client_names.json 的读取大小（正常约 1 MB）。
const maxClientNamesBytes = 32 << 20

type clientNames struct {
	Names map[string]string `json:"names"` // systemId（item_id / fish_key）→ 英文名
	ZH2EN map[string]string `json:"zh2en"` // 客户端 zh_CN 名 → 英文名
}

// bomRune 是部分静态库鱼名末尾残留的 U+FEFF。
const bomRune = string(rune(0xFEFF))

var hasCJK = regexp.MustCompile(`[\p{Han}]`)

// namesCache 按 ref 文件的修改时间与大小缓存生成结果；重新导出后自动刷新。
type namesCache struct {
	mu      sync.Mutex
	key     string
	payload []byte
}

// loadClientNames 读取 ref/client_names.json；文件不存在时返回 (nil, "", nil)。
func loadClientNames() (*clientNames, string, error) {
	path, ok := findRefAsset("", clientNamesFile)
	if !ok {
		return nil, "", nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, "", err
	}
	if info.Size() > maxClientNamesBytes {
		return nil, "", fmt.Errorf("%s 过大（%d 字节）", clientNamesFile, info.Size())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var cn clientNames
	if err := json.Unmarshal(b, &cn); err != nil {
		return nil, "", fmt.Errorf("解析 %s: %w", clientNamesFile, err)
	}
	return &cn, fmt.Sprintf("%s|%d|%d", path, info.Size(), info.ModTime().UnixNano()), nil
}

// buildNamesDict 生成「中文名 → 英文名」：客户端 zh2en 词条，加上数据库里按
// item_id / fish_key 能对上英文名的中文名称。键去掉首尾空白与 BOM 后也各存一份。
func (s *Server) buildNamesDict(ctx context.Context, cn *clientNames) map[string]string {
	out := map[string]string{}
	put := func(zh, en string) {
		en = strings.TrimSpace(en)
		if en == "" || !hasCJK.MatchString(zh) || hasCJK.MatchString(en) {
			return
		}
		for _, k := range []string{zh, strings.TrimSpace(strings.Trim(zh, bomRune))} {
			if _, exists := out[k]; !exists && k != "" {
				out[k] = en
			}
		}
	}
	if cn == nil {
		return out
	}
	for zh, en := range cn.ZH2EN {
		put(zh, en)
	}
	if s.db == nil || len(cn.Names) == 0 {
		return out
	}
	queries := []string{
		`SELECT item_id, name_zh FROM item_catalog WHERE name_zh GLOB '*[一-龥]*'`,
		`SELECT DISTINCT fish_key, fish_zh FROM loc_wiki_fish`,
		`SELECT DISTINCT fish_key, name_zh FROM loc_fish`,
	}
	for _, q := range queries {
		rows, err := s.db.SQL.QueryContext(ctx, q)
		if err != nil {
			s.log.l.Warn("英文名称：查询失败", "err", err)
			continue
		}
		for rows.Next() {
			var id, zh string
			if rows.Scan(&id, &zh) == nil {
				put(zh, cn.Names[id])
			}
		}
		_ = rows.Close()
	}
	return out
}

// namesScript 返回 i18n/names_en.js 的内容：window.RF4I18N_NAMES = {...}。
// 没有 ref/client_names.json 时下发空字典（页面照常，名称保持中文）。
func (s *Server) namesScript(ctx context.Context) []byte {
	cn, key, err := loadClientNames()
	if err != nil {
		s.log.l.Warn("英文名称：读取 ref/"+clientNamesFile+" 失败", "err", err)
	}
	s.names.mu.Lock()
	defer s.names.mu.Unlock()
	if s.names.payload != nil && s.names.key == key {
		return s.names.payload
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	dict, _ := json.Marshal(s.buildNamesDict(ctx, cn))
	s.names.payload = []byte("window.RF4I18N_NAMES=" + string(dict) + ";\n")
	s.names.key = key
	return s.names.payload
}
