package gameserver

// 酒兴播报：喝酒当刻与心跳定时把「当前喝了几瓶、buff 还剩多久」推到聊天框。
//
// 与耐力播报（gear_assistant_stamina.go）同通道：24/27 公聊回显，kind=0，
// 只对本人可见（users=1）。开关存 kv，默认关闭，挂在钓具助手第 3 项。
//
// 数据来自逐瓶表 drink_bonus_effect（每瓶一条、各自倒计时，最多 10 瓶）：
//   - 瓶数 = 仍有效的行数（remaining_millis>0）；
//   - 剩余时间 = 所有瓶中最大的 remaining_millis（最早喝的那瓶先到期，
//     但加成是逐瓶相加的，所以「buff 还能持续多久」取最长那瓶）。
//   - 官服酒表时长是游戏小时，但这里按用户口径只显示现实分秒，即数据库中的
//     remaining_millis 换算出的 mm:ss，不做 24 倍速折算。
//
// 剩余时间为 0（所有瓶到期）时不推送，也不推「已结束」——避免每次到期刷屏。

import (
	"fmt"
	"time"
)

// maybePushDrinkReport 读取开关与当前逐瓶状态，按需推送一条播报。
// 开关默认为关（kv 无行即关），关时不查瓶状态、不推送。
func (s *Server) maybePushDrinkReport(sess *Session, now time.Time) bool {
	if s == nil || sess == nil || s.DB == nil || sess.Userid <= 0 || sess.Closed() {
		return false
	}
	on, err := readKVFlag(sess.pushCtx(), s.DB.SQL, drinkReportKey(sess.Userid))
	if err != nil || !on {
		return false
	}
	statuses, err := s.DB.DrinkBonusStatuses(sess.pushCtx(), sess.Userid)
	if err != nil {
		return false
	}
	var longest time.Duration
	for _, st := range statuses {
		if d := time.Duration(st.RemainingMillis) * time.Millisecond; d > longest {
			longest = d
		}
	}
	return sess.emitDrinkReport(drinkReportText(len(statuses), longest), now)
}

// emitDrinkReport 去重后推送。文本相同且距上次不足一个刷新间隔就跳过——
// 这样喝酒当刻与紧接着的心跳不会连续刷两条相同内容；瓶数变化会改变文本，
// 因而连喝时每瓶都会即时播报。返回是否真正推送。
func (sess *Session) emitDrinkReport(text string, now time.Time) bool {
	if text == "" {
		return false
	}
	sess.drinkReportMu.Lock()
	defer sess.drinkReportMu.Unlock()
	if text == sess.drinkReportText && !sess.drinkReportAt.IsZero() &&
		now.Sub(sess.drinkReportAt) < drinkReportInterval {
		return false
	}
	if sess.Push(24, 27, buildChatEchoPush("", text, uint64(sess.Userid), 1)) == 0 {
		return false
	}
	sess.drinkReportText, sess.drinkReportAt = text, now
	return true
}

const (
	drinkReportKeyFmt   = "gear-assistant:drink-report:%d"
	drinkReportInterval = 30 * time.Second // 心跳里约每 30 秒刷新一次
)

func drinkReportKey(uid int64) string {
	return fmt.Sprintf(drinkReportKeyFmt, uid)
}

// drinkReportText 把有效瓶的剩余时间格式化为播报文本。空切片返回空串（不播报）。
// 秒向上取整，避免刚喝下显示「剩余 0分00秒」。
func drinkReportText(bottles int, longest time.Duration) string {
	if bottles <= 0 || longest <= 0 {
		return ""
	}
	totalSec := int64((longest + time.Second - 1) / time.Second)
	if totalSec <= 0 {
		return ""
	}
	return fmt.Sprintf("[酒兴] 已喝 %d 瓶 · 剩余 %02d分%02d秒", bottles, totalSec/60, totalSec%60)
}
