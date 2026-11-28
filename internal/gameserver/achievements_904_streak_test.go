package gameserver

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"rf4server/internal/logging"
	"rf4server/internal/proto"
)

// 904「熟能生巧」：连续钓五条鱼不断档才解锁。
// 连续数存在 player_counters 的 catch_streak 上：成功上鱼结算 +1（回执重放不算），
// 中断（escapes / line_breaks / component_breaks / rod_breaks）在 bumpFishingCounter
// 里归零。中断点只有 bumpFishingCounter 一个入口，见 TestFishingInterruptionCountersCoverBumpSites。
const test904UID = int64(1000041)

// landFor904 走生产路径累加连续上鱼数，再按同一份数据判定一次上鱼事件，
// 与收竿结算里的装配一致（progress_ops.go fishingEndSettle 的 evFishLanded + Streak）。
func landFor904(t *testing.T, env *dlineTestEnv, uid int64) int64 {
	t.Helper()
	streak, err := env.srv.bumpCatchStreak(uid)
	if err != nil {
		t.Fatalf("累加连续上鱼数失败: %v", err)
	}
	c := env.ctx(14, 4, nil)
	env.srv.evaluateAchievements(c, achievementEvent{Kind: evFishLanded, WeightG: 300, Streak: streak})
	return streak
}

// achievement904 读 3/3 里的 904 状态。
func achievement904(t *testing.T, env *dlineTestEnv) achievementDTO {
	t.Helper()
	return achievementsByID(t, env.invoke(t, 3, 3, nil))[904]
}

// 连续四条不解锁（覆盖「只钓 4 条 → 不解锁」），第五条解锁并点亮。
func TestAchievement904NeedsFiveConsecutiveCatches(t *testing.T) {
	const uid = test904UID
	env := newDLineTestEnv(t, uid)
	registerAchievementHandlers(env.srv.dispatch)

	for i := 1; i <= 4; i++ {
		if got := landFor904(t, env, uid); got != int64(i) {
			t.Fatalf("第 %d 条上鱼后连续数期望 %d，实得 %d", i, i, got)
		}
		if st := achievement904(t, env); st.unlocked || st.claimed || st.progress != 0 {
			t.Fatalf("连续 %d 条时 904 不该解锁：期望 locked/progress=0，实得 %+v", i, st)
		}
	}
	if got := landFor904(t, env, uid); got != 5 {
		t.Fatalf("第 5 条上鱼后连续数期望 5，实得 %d", got)
	}
	if st := achievement904(t, env); !st.unlocked || !st.claimed || st.progress != 1 {
		t.Fatalf("连续 5 条时 904 应解锁并点亮：期望 unlocked/claimed/progress=1，实得 %+v", st)
	}
}

// 钓 4 条 → 跑一次鱼（14/12 真脱钩）→ 再钓 1 条：连续数已归零，不解锁。
func TestAchievement904ResetsAfterEscape(t *testing.T) {
	const uid = test904UID
	env := newDLineTestEnv(t, uid)
	registerAchievementHandlers(env.srv.dispatch)
	if _, err := env.db.EnsureProfile(context.Background(), uid); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		landFor904(t, env, uid)
	}

	// 真脱钩：14/12 f0=2 走 hFishingLeft 的 escapes 分支。
	rod := strings.Repeat("a", 32)
	fish := &RolledFish{FishGUID: strings.Repeat("5", 32), Species: "perch", WeightG: 640}
	st := env.srv.stateFor(uid)
	st.mu.Lock()
	st.CastActive, st.Pending, st.Hooked = true, fish, true
	st.mu.Unlock()
	if _, ok := hFishingLeft(miscCtx(env.sess, 14, 12, leftTree(rod, fish.FishGUID, 2))); !ok {
		t.Fatal("14/12 真脱钩帧未处理")
	}
	if got := counterValue(t, env.db, uid, "escapes"); got != 1 {
		t.Fatalf("脱钩后 escapes 期望 1，实得 %d", got)
	}
	if got := counterValue(t, env.db, uid, catchStreakKey); got != 0 {
		t.Fatalf("脱钩后连续上鱼数应归零：期望 0，实得 %d", got)
	}
	if st := achievement904(t, env); st.unlocked {
		t.Fatalf("中断后 904 不该解锁：实得 %+v", st)
	}

	if got := landFor904(t, env, uid); got != 1 {
		t.Fatalf("中断后再钓一条，连续数期望 1，实得 %d", got)
	}
	if st := achievement904(t, env); st.unlocked || st.claimed {
		t.Fatalf("中断后再钓一条 904 仍不该解锁：实得 %+v", st)
	}
}

// 三种中断逐一验证都会清零：escapes / line_breaks / rod_breaks 各走真实处理器，
// 先确认对应的中断计数确实 +1（证明中断事件真的发生了），再确认连续数归零。
// 断线与断竿要读 item_def 静态表，用例统一用带静态物品库的测试服务器。
func TestAchievement904StreakClearedByEachInterruption(t *testing.T) {
	dropFish := func(t *testing.T, s *Server, sess *Session, uid int64) {
		t.Helper()
		rod := strings.Repeat("a", 32)
		fish := &RolledFish{FishGUID: strings.Repeat("5", 32), Species: "perch", WeightG: 640}
		st := s.stateFor(uid)
		st.mu.Lock()
		st.CastActive, st.Pending, st.Hooked = true, fish, true
		st.mu.Unlock()
		if _, ok := hFishingLeft(miscCtx(sess, 14, 12, leftTree(rod, fish.FishGUID, 2))); !ok {
			t.Fatal("14/12 真脱钩帧未处理")
		}
	}
	breakLine := func(t *testing.T, s *Server, sess *Session, uid int64) {
		t.Helper()
		ctx := context.Background()
		rod, err := s.CreateItem(ctx, uid, "tele_10180_4", "backpack", 1)
		if err != nil {
			t.Fatalf("造竿失败: %v", err)
		}
		line, err := s.CreateItem(ctx, uid, "mono_15095", "backpack", 1)
		if err != nil {
			t.Fatalf("造线失败: %v", err)
		}
		mountGearForTest(t, s, sess, rod.GUID, line.GUID)
		tree := rigRequestTree(t, proto.Guid(rod.GUID), proto.Guid(line.GUID), proto.U32(1), proto.Null())
		body, ok := hDynamicRodBreak(&Ctx{Sess: sess, Main: 17, Sub: 9, Tree: tree,
			DecodeOK: true, ExpectResponse: true, Log: logging.Discard()})
		if !ok || len(body) == 0 {
			t.Fatal("17/9 主线断裂未处理")
		}
	}
	breakRod := func(t *testing.T, s *Server, sess *Session, uid int64) {
		t.Helper()
		rod, err := s.CreateItem(context.Background(), uid, "feeder_10125_FD360", "backpack", 1)
		if err != nil {
			t.Fatalf("造竿失败: %v", err)
		}
		tree := rigRequestTree(t, proto.Guid(rod.GUID), proto.Guid(rod.GUID), proto.U32(1), proto.Null())
		body, ok := hDynamicRodBreak(&Ctx{Sess: sess, Main: 17, Sub: 9, Tree: tree,
			DecodeOK: true, ExpectResponse: true, Log: logging.Discard()})
		if !ok || len(body) == 0 {
			t.Fatal("17/9 断竿未处理")
		}
	}

	for _, tc := range []struct {
		name    string
		counter string
		trigger func(t *testing.T, s *Server, sess *Session, uid int64)
	}{
		{"escapes-脱钩", "escapes", dropFish},
		{"line_breaks-断线", "line_breaks", breakLine},
		{"rod_breaks-断竿", "rod_breaks", breakRod},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, db := newRigTestServer(t)
			sess := newProbeSess(s)
			uid := sess.Userid
			if _, err := db.EnsureProfile(context.Background(), uid); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 4; i++ {
				if _, err := s.bumpCatchStreak(uid); err != nil {
					t.Fatalf("累加连续上鱼数失败: %v", err)
				}
			}
			if got := counterValue(t, db, uid, catchStreakKey); got != 4 {
				t.Fatalf("前置条件失败：连续数期望 4，实得 %d", got)
			}

			tc.trigger(t, s, sess, uid)

			if got := counterValue(t, db, uid, tc.counter); got != 1 {
				t.Fatalf("%s 中断计数期望 1，实得 %d（中断事件没有真正发生，本用例不成立）", tc.counter, got)
			}
			if got := counterValue(t, db, uid, catchStreakKey); got != 0 {
				t.Fatalf("%s 中断后连续上鱼数应归零：期望 0，实得 %d", tc.counter, got)
			}
		})
	}
}

// 同一尾鱼重复收竿结算（回执重放）不再累加连续上鱼数：
// 第一次结算 +1，第二次重放保持 1，否则反复重放能刷出 904。
func TestAchievement904ReplayDoesNotAddStreak(t *testing.T) {
	s, db := newRigTestServer(t)
	sess := newProbeSess(s)
	uid := sess.Userid
	if _, err := db.EnsureProfile(t.Context(), uid); err != nil {
		t.Fatal(err)
	}
	fish := &RolledFish{FishGUID: strings.Repeat("c", 32), Species: "perch", Grade: 1, WeightG: 640}
	st := s.stateFor(uid)
	st.mu.Lock()
	st.Pending, st.Hooked = fish, true
	st.mu.Unlock()
	settle := func(call uint32) {
		t.Helper()
		body, ok := s.fishingEndSettle(&Ctx{Sess: sess, CallID: call, Main: 14, Sub: 4,
			ExpectResponse: true, Log: logging.Discard()})
		if !ok || len(body) == 0 {
			t.Fatalf("第 %d 次收竿结算未返回", call)
		}
	}

	settle(1)
	if got := counterValue(t, db, uid, catchStreakKey); got != 1 {
		t.Fatalf("首次结算后连续数期望 1，实得 %d", got)
	}
	settle(2)
	if got := counterValue(t, db, uid, catchStreakKey); got != 1 {
		t.Fatalf("重放结算不得再加连续数：期望 1，实得 %d", got)
	}
}

// 计数读取失败不能当成 0：写入已把连续数推到 5，但这次判定必须跳过并留下 warn，
// 否则玩家会静默漏解锁，且日志里看不出原因。
func TestAchievement904CountFaultSkipsEvaluation(t *testing.T) {
	s, baseDB := newRigTestServer(t)
	fault := errors.New("注入：读取 catch_streak 失败")
	db := openRareCountFaultDB(t, baseDB.Path, fault)
	s.SetDB(db)

	sess := newProbeSess(s)
	uid := sess.Userid
	if _, err := db.EnsureProfile(t.Context(), uid); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO player_counters(uid,key,value) VALUES(?,?,4)`,
		uid, catchStreakKey); err != nil {
		t.Fatal(err)
	}

	fish := &RolledFish{FishGUID: strings.Repeat("f", 32), Species: "perch", Grade: 1, WeightG: 640}
	st := s.stateFor(uid)
	st.mu.Lock()
	st.Pending, st.Hooked = fish, true
	st.mu.Unlock()

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	body, handled := s.fishingEndSettle(&Ctx{Sess: sess, CallID: 1, Main: 14, Sub: 4,
		ExpectResponse: true, Log: log})
	if !handled || len(body) == 0 {
		t.Fatalf("计数读取失败不应阻断结算，handled=%v body=%x", handled, body)
	}
	if !strings.Contains(logs.String(), "连续上鱼计数读取失败，跳过成就判定") {
		t.Fatalf("计数读取失败必须留下 warn，实际日志: %q", logs.String())
	}
	if got := counterValue(t, baseDB, uid, catchStreakKey); got != 5 {
		t.Fatalf("读回失败前应先写入第 5 条连续计数，期望 5，实得 %d", got)
	}
	var unlocked int
	if err := baseDB.SQL.QueryRow(`SELECT COUNT(*) FROM player_achievements WHERE uid=? AND ach_id=? AND unlocked=1`,
		uid, 904).Scan(&unlocked); err != nil {
		t.Fatal(err)
	}
	if unlocked != 0 {
		t.Fatalf("读取失败时不能判定 904 解锁，实得 %d 行", unlocked)
	}
}

// 多钩一杆虽有多尾 catch_log，但 14/4 只有一个 evFishLanded，连续数也只 +1。
// 否则同一杆挂三条鱼会直接把 904 刷出来，与"连续五条鱼"的口径不符。
func TestAchievement904CountsMultiHookReelInOnce(t *testing.T) {
	s, db := newRigTestServer(t)
	sess := newProbeSess(s)
	uid := sess.Userid
	if _, err := db.EnsureProfile(t.Context(), uid); err != nil {
		t.Fatal(err)
	}

	main := &RolledFish{FishGUID: strings.Repeat("a", 32), Species: "perch", Grade: 2, WeightG: 640}
	extra1 := &RolledFish{FishGUID: strings.Repeat("b", 32), Species: "perch", Grade: 1, WeightG: 320}
	extra2 := &RolledFish{FishGUID: strings.Repeat("c", 32), Species: "perch", Grade: 1, WeightG: 210}
	st := s.stateFor(uid)
	st.mu.Lock()
	st.Pending, st.Hooked = main, true
	st.Extra = []*RolledFish{extra1, extra2}
	st.mu.Unlock()

	if _, ok := s.fishingEndSettle(&Ctx{Sess: sess, CallID: 1, Main: 14, Sub: 4,
		ExpectResponse: true, Log: logging.Discard()}); !ok {
		t.Fatal("结算未正常返回")
	}
	if got := counterValue(t, db, uid, catchStreakKey); got != 1 {
		t.Fatalf("多钩一杆应只累加一次连续上鱼数，期望 1，实得 %d", got)
	}
	var logged int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM catch_log WHERE uid=?`, uid).Scan(&logged); err != nil {
		t.Fatal(err)
	}
	if logged != 3 {
		t.Fatalf("前置条件失败：三尾鱼都应写入 catch_log，实得 %d", logged)
	}
}

// 机器证据：生产代码里每个 bumpFishingCounter 的计数键，无论是否算中断，都必须在
// expectedCalls 里按调用点数登记；中断语义的还必须进 fishingInterruptionCounters。
// 新增一个中断计数却忘了登记时本用例变红，这就是「没有漏掉中断点」的检查方式。
func TestFishingInterruptionCountersCoverBumpSites(t *testing.T) {
	// 锁定当前 8 个生产调用点及其计数键。只看"出现过的键"不够：把一处 escapes
	// 误写成非中断键时，键名集合不变，测试会漏掉这条真实中断路径。
	// reel_breaks 固定为 0：渔轮损坏但鱼仍在钩上（KeepFight），真正丢鱼另计
	// escapes/line_breaks。
	expectedCalls := map[string]int{
		"escapes":          4,
		"line_breaks":      2,
		"component_breaks": 1,
		"rod_breaks":       1,
		"reel_breaks":      0,
	}
	for _, key := range []string{"escapes", "line_breaks", "component_breaks", "rod_breaks"} {
		if !fishingInterruptionCounters[key] {
			t.Errorf("中断计数 %q 未登记在 fishingInterruptionCounters，中断不会清零连续上鱼数", key)
		}
	}

	re := regexp.MustCompile(`bumpFishingCounter\([^,]+,\s*"([a-z_]+)"\)`)
	counts := map[string]int{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
			counts[m[1]]++
		}
	}
	if len(counts) == 0 {
		t.Fatal("没有扫描到任何 bumpFishingCounter 调用点：扫描规则已失效，本用例不再有证明力")
	}
	for key, want := range expectedCalls {
		if got := counts[key]; got != want {
			t.Errorf("bumpFishingCounter(%q) 调用点数 = %d, want %d", key, got, want)
		}
	}
	for key := range counts {
		if _, ok := expectedCalls[key]; !ok {
			t.Errorf("计数键 %q 既未登记为中断，也未登记为非中断：新调用点必须更新 expectedCalls", key)
		}
	}
	for key := range fishingInterruptionCounters {
		if counts[key] == 0 {
			t.Errorf("fishingInterruptionCounters 里的 %q 没有任何 bumpFishingCounter 调用点：登记已过期", key)
		}
	}
}
