package gameserver

import (
	"context"
	"fmt"
	"time"

	"rf4server/internal/config"
)

// 服务端拼装的聊天文案按 game.language 二选一（见 specs/changes/ui-english-i18n）。
// 中文文案保持原字面量，英文为并列分支；未配置（测试里直接构造的 config.Game）
// 按中文处理，保持旧行为。

// langEN 是否使用英文文案。
func (s *Server) langEN() bool {
	return s != nil && s.cfg.Language == config.LanguageEN
}

// tr 按当前语言二选一。
func (s *Server) tr(zh, en string) string {
	if s.langEN() {
		return en
	}
	return zh
}

// speciesChatName 聊天显示用鱼名：英文服取官方英文名，缺失回退中文名，再回退 key。
func (s *Server) speciesChatName(ctx context.Context, key string) string {
	if s.langEN() {
		if name, ok := fishNamesEN[key]; ok {
			return name
		}
	}
	return s.speciesZhName(ctx, key)
}

// gradeChatName 档位显示名（1 普通 / 2 达标 / 3 奖杯 / 4 蓝冠）。
func (s *Server) gradeChatName(grade int64) string {
	if !s.langEN() {
		return gradeZhName(grade)
	}
	switch grade {
	case 2:
		return "qualifying"
	case 3:
		return "trophy"
	case 4:
		return "unique trophy"
	}
	return "regular"
}

// drinkReportTextEN drinkReportText 的英文版，取整与空串规则一致。
func drinkReportTextEN(bottles int, longest time.Duration) string {
	if drinkReportText(bottles, longest) == "" {
		return ""
	}
	totalSec := int64((longest + time.Second - 1) / time.Second)
	return fmt.Sprintf("[Drink] %d bottle(s) · %02dm%02ds left", bottles, totalSec/60, totalSec%60)
}
