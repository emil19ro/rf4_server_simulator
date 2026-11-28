package gameserver

// 成就 210「刺激它们的食欲」：抛掷装备 PVA 棒的钓组。
//
// PVA 棒是挂在钓组上的饵料载荷（品类 30），不是钓组类型（rigmeta.go 的
// rigTypeNames 里没有 PVA），所以判据不能像 206 抛饵笼/211 测竿那样近似成
// rigType，而是取「本杆入水确实消耗掉一份 PVA 棒」的扣料回执。

import (
	"testing"

	"rf4server/internal/logging"
	"rf4server/internal/proto"
	"rf4server/internal/wiregen"
)

// achievementStates210 通过 3/3 读出当前玩家的成就状态表。
func achievementStates210(t *testing.T, s *Server, sess *Session) map[int64]achievementDTO {
	t.Helper()
	h := s.dispatch.lookup(3, 3)
	if h == nil {
		t.Fatal("3/3 成就处理器未注册")
	}
	body, ok := h(&Ctx{Sess: sess, Main: 3, Sub: 3, Log: logging.Discard()})
	if !ok {
		t.Fatal("3/3 未处理")
	}
	v, _, err := proto.DecodeValue(body, 0, wiregen.Table())
	if err != nil {
		t.Fatalf("解码 3/3 响应失败: %v", err)
	}
	return achievementsByID(t, v)
}

// pvaAchievementRig 准备一根可抛投的沉底竿（rig 12）和与之绑定的会话。
func pvaAchievementRig(t *testing.T, s *Server) (*Session, string) {
	t.Helper()
	sess, _ := newTunnelProbe(t, s)
	s.sessions[sess.ID] = sess
	rod := baitTestRod(t, s)
	mountHookForBaitTest(t, s, rod, 0)
	return sess, rod
}

// castRigToWater 完成一次抛投并触发 14/7 的入水动画位，让入水扣料真的发生。
func castRigToWater(t *testing.T, s *Server, sess *Session, rod string) *FishState {
	t.Helper()
	st := s.fishingStateForRod(sess.Userid, rod)
	st.mu.Lock()
	st.GearGuid = rod
	st.Loc, st.CastPosX, st.CastPosZ, st.HasCastPos = "pva-achievement-loc", 100, 100, true
	st.mu.Unlock()
	if !s.scheduleBite(sess, st) {
		t.Fatal("排咬钩失败")
	}
	fields := make([]proto.Field, 7)
	fields[5] = proto.Field{Op: "U32", Num: ptrU64(0x400)} // 0x400 = 钓组在水中
	fields[6] = proto.Field{Op: "STR", Str: strPtr("pva-achievement-loc")}
	if _, ok := hFishingStep(&Ctx{Sess: sess, Main: 14, Sub: 7,
		Tree:     proto.Args524(proto.Obj("lobemkloehp", 0, fields...)),
		DecodeOK: true, Log: logging.Discard()}); !ok {
		t.Fatal("14/7 未处理")
	}
	return st
}

// landFishOnRig 让本杆结算一尾鱼，返回结算后的成就状态表。
func landFishOnRig(t *testing.T, s *Server, sess *Session, st *FishState, call uint32) map[int64]achievementDTO {
	t.Helper()
	st.mu.Lock()
	st.Pending = &RolledFish{FishGUID: newFishGUID(), Species: "crucian", Grade: 1, WeightG: 500, LengthM: .25}
	st.Hooked, st.Resolved, st.PushSent = true, false, true
	st.mu.Unlock()
	body, handled := s.fishingEndSettle(&Ctx{Sess: sess, CallID: call, Main: 14, Sub: 4,
		ExpectResponse: true, Log: logging.Discard()})
	if !handled || len(body) == 0 {
		t.Fatalf("14/4 结算未返回: handled=%v len=%d", handled, len(body))
	}
	return achievementStates210(t, s, sess)
}

// 装备 PVA 棒抛投并上鱼：入水扣掉那份 PVA，成就 210 解锁。
func TestAchievement210PVAStickCastLanded(t *testing.T) {
	s, db := newRigTestServer(t)
	registerAchievementHandlers(s.dispatch)
	t.Cleanup(func() { stopFishingRecoveryTimers(s) })
	ctx := t.Context()
	sess, rod := pvaAchievementRig(t, s)

	_, child := baitTestMount(t, s, rod, "pva_stick_m", 1, 0) // 品类 30，挂在钓组上
	st := castRigToWater(t, s, sess, rod)
	if row, _ := db.GetItemByGUID(ctx, child); row != nil {
		t.Fatalf("入水后 PVA 棒应已被消耗，实际仍存在 %+v", row)
	}
	st.mu.Lock()
	ready := st.PVAReady
	st.mu.Unlock()
	if !ready {
		t.Fatalf("入水扣掉一份 PVA 棒后 PVAReady 应为 true，实得 false")
	}

	got := landFishOnRig(t, s, sess, st, 1)
	if !got[210].unlocked || !got[210].claimed {
		t.Errorf("装备 PVA 棒抛投并上鱼后成就 210 应解锁，实际 %+v", got[210])
	}
}

// 咬钩前服务端重启仍恢复同一杆的咬钩 timer；PVA 标记必须随快照恢复，
// 否则这一杆上鱼时 210 会静默漏判。
func TestAchievement210PVAReadySurvivesServerRestart(t *testing.T) {
	s, db := newRigTestServer(t)
	registerAchievementHandlers(s.dispatch)
	t.Cleanup(func() { stopFishingRecoveryTimers(s) })
	ctx := t.Context()
	sess, rod := pvaAchievementRig(t, s)

	baitTestMount(t, s, rod, "pva_stick_m", 1, 0)
	castRigToWater(t, s, sess, rod)
	saved, err := db.LoadFishingState(ctx, sess.Userid)
	if err != nil {
		t.Fatal(err)
	}
	if saved == nil || !saved.PVAReady {
		t.Fatalf("入水后快照应保存 PVAReady=true，实得 %+v", saved)
	}

	restarted := newFishingRecoveryServer(t, db)
	registerAchievementHandlers(restarted.dispatch)
	restartedSess := newProbeSess(restarted)
	rst := restarted.fishingStateForRod(restartedSess.Userid, rod)
	rst.mu.Lock()
	ready := rst.PVAReady
	rst.mu.Unlock()
	if !ready {
		t.Fatal("重启恢复后 PVAReady 应为 true")
	}

	got := landFishOnRig(t, restarted, restartedSess, rst, 1)
	if !got[210].unlocked || !got[210].claimed {
		t.Errorf("重启恢复的 PVA 杆上鱼后成就 210 应解锁，实际 %+v", got[210])
	}
}

// 没有 PVA 棒：无窝料或只挂普通窝料（品类 46）抛投上鱼都不解锁 210。
func TestAchievement210WithoutPVAStickNotUnlocked(t *testing.T) {
	// 两个场景各自起一套环境：14/4 结算后本杆进入「待入护/放生」窗口，
	// 同一根竿不能再抛第二杆，分开建环境才能各验一次抛投。
	cases := []struct {
		name string
		bait string // 空 = 钓组上什么都不挂
	}{
		{"无窝料", ""},
		{"普通窝料", "feedcompleted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newRigTestServer(t)
			registerAchievementHandlers(s.dispatch)
			t.Cleanup(func() { stopFishingRecoveryTimers(s) })
			sess, rod := pvaAchievementRig(t, s)
			if tc.bait != "" {
				baitTestMount(t, s, rod, tc.bait, 1, 0)
			}

			st := castRigToWater(t, s, sess, rod)
			st.mu.Lock()
			ready := st.PVAReady
			st.mu.Unlock()
			if ready {
				t.Fatalf("%s 抛投时 PVAReady 应为 false，实得 true", tc.name)
			}
			got := landFishOnRig(t, s, sess, st, 1)
			if got[210].unlocked {
				t.Errorf("%s 抛投上鱼不应解锁 210，实际 %+v", tc.name, got[210])
			}
		})
	}
}

// PVA 棒只有一份：第一杆入水就用完，第二杆没有 PVA 可挂，不再计入 210。
func TestAchievement210ConsumedPVANextCastNotCounted(t *testing.T) {
	s, db := newRigTestServer(t)
	registerAchievementHandlers(s.dispatch)
	t.Cleanup(func() { stopFishingRecoveryTimers(s) })
	ctx := t.Context()
	sess, rod := pvaAchievementRig(t, s)

	_, child := baitTestMount(t, s, rod, "pva_stick_s", 1, 0)
	castRigToWater(t, s, sess, rod)
	if row, _ := db.GetItemByGUID(ctx, child); row != nil {
		t.Fatalf("第一杆入水应消耗掉唯一的 PVA 棒，实际仍存在 %+v", row)
	}

	// 下一杆补饵：PVA 已用完，补饵不得凭空复活一份。
	if _, err := s.reloadBaitForCast(ctx, sess.Userid, rod); err != nil {
		t.Fatalf("下一杆补饵返回错误: %v", err)
	}
	for _, comp := range s.storedRigComponents(ctx, rod) {
		if comp.Category == catPVAStick {
			t.Fatalf("PVA 用完后钓组上仍有品类 30 组件: %+v", comp)
		}
	}

	st2 := castRigToWater(t, s, sess, rod)
	st2.mu.Lock()
	ready := st2.PVAReady
	st2.mu.Unlock()
	if ready {
		t.Fatalf("PVA 已用完的第二杆 PVAReady 应为 false，实得 true")
	}
	got := landFishOnRig(t, s, sess, st2, 1)
	if got[210].unlocked {
		t.Errorf("PVA 用完后上鱼不应解锁 210，实际 %+v", got[210])
	}
}
