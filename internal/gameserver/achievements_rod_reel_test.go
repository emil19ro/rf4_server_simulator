package gameserver

// 竿型/轮型成就（102/104/156/157/204/209）：
//   规则表只做一条布尔判断，事件里的 RodType/ReelType 由 14/4 装配点从
//   castDistanceForRig 的同一次竿定义解析带出来（见 progress_ops.go）。
//   这里分两层验证：规则层正/反例不串台，装配层能真的把子类解析出来。

import (
	"context"
	"testing"

	"rf4server/internal/logging"
)

// 六个目标成就：竿法子类或轮法子类命中才解锁。
//
// 157 用真装配的枪柄竿 + 抛饵卷线器：卷线器只能装在枪柄竿上，所以同一次
// 上鱼按设计会连带点亮 156（枪柄竿）。这是两条独立规则各判各的，不是串台，
// 因此 156 放在 both 里单独断言，不算进 others 的「不应解锁」。
var rodReelAchievements = []struct {
	id     int64
	name   string
	ev     achievementEvent
	others []int64
	both   []int64
}{
	{102, "博洛尼亚竿", achievementEvent{Kind: evFishLanded, WeightG: 300, RodType: 4}, []int64{104, 156, 157, 204, 209}, nil},
	{104, "竞赛竿", achievementEvent{Kind: evFishLanded, WeightG: 300, RodType: 5}, []int64{102, 156, 157, 204, 209}, nil},
	{156, "枪柄竿", achievementEvent{Kind: evFishLanded, WeightG: 300, RodType: 2}, []int64{102, 104, 157, 204, 209}, nil},
	{157, "抛饵卷线器", achievementEvent{Kind: evFishLanded, WeightG: 300, RodType: 2, ReelType: 2}, []int64{102, 104, 204, 209}, []int64{156}},
	{204, "PICKER 竿", achievementEvent{Kind: evFishLanded, WeightG: 300, RodType: 9}, []int64{102, 104, 156, 157, 209}, nil},
	{209, "鲤鱼竿", achievementEvent{Kind: evFishLanded, WeightG: 300, RodType: 11}, []int64{102, 104, 156, 157, 204}, nil},
}

// 正例：对应子类的竿/轮上鱼，只点亮这一条成就。
func TestAchievementRodReelTypeUnlocks(t *testing.T) {
	for i, tc := range rodReelAchievements {
		t.Run(tc.name, func(t *testing.T) {
			env := newDLineTestEnv(t, int64(1000100+i))
			registerAchievementHandlers(env.srv.dispatch)

			env.srv.evaluateAchievements(env.ctx(14, 4, nil), tc.ev)
			got := achievementsByID(t, env.invoke(t, 3, 3, nil))
			if !got[tc.id].unlocked || !got[tc.id].claimed {
				t.Errorf("使用%s上鱼后成就 %d 未解锁: %+v", tc.name, tc.id, got[tc.id])
			}
			for _, other := range tc.others {
				if got[other].unlocked {
					t.Errorf("只用了%s，成就 %d 不应解锁: %+v", tc.name, other, got[other])
				}
			}
			for _, both := range tc.both {
				if !got[both].unlocked {
					t.Errorf("用%s上鱼时成就 %d 应同时解锁: %+v", tc.name, both, got[both])
				}
			}
		})
	}
}

// 反例：换其它子类的竿/轮不上对应成就，证明不会串台。
func TestAchievementRodReelTypeOtherSubtypeStaysLocked(t *testing.T) {
	cases := []struct {
		name string
		ev   achievementEvent
		want []int64 // 必须仍锁定的目标成就
	}{
		{"纺车竿+纺车轮", achievementEvent{Kind: evFishLanded, WeightG: 300, RodType: 1, ReelType: 1}, []int64{102, 104, 156, 157, 204, 209}},
		{"飞德竿+鼓轮", achievementEvent{Kind: evFishLanded, WeightG: 300, RodType: 10, ReelType: 3}, []int64{102, 104, 156, 157, 204, 209}},
		{"枪柄竿配传统轮", achievementEvent{Kind: evFishLanded, WeightG: 300, RodType: 2, ReelType: 4}, []int64{102, 104, 157, 204, 209}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newDLineTestEnv(t, int64(1000110+i))
			registerAchievementHandlers(env.srv.dispatch)

			env.srv.evaluateAchievements(env.ctx(14, 4, nil), tc.ev)
			got := achievementsByID(t, env.invoke(t, 3, 3, nil))
			for _, id := range tc.want {
				if got[id].unlocked {
					t.Errorf("%s 不应解锁成就 %d: %+v", tc.name, id, got[id])
				}
			}
		})
	}
}

// 解析失败（无轮手竿 / 无效竿 GUID）时 RodType/ReelType 为 0，不 panic 也不误解锁。
func TestAchievementRodReelTypeZeroUnlocksNothing(t *testing.T) {
	env := newDLineTestEnv(t, 1000120)
	registerAchievementHandlers(env.srv.dispatch)

	if _, err := env.db.EnsureProfile(context.Background(), 1000120); err != nil {
		t.Fatal(err)
	}
	env.srv.evaluateAchievements(env.ctx(14, 4, nil), achievementEvent{Kind: evFishLanded, WeightG: 300})
	got := achievementsByID(t, env.invoke(t, 3, 3, nil))
	for _, tc := range rodReelAchievements {
		if got[tc.id].unlocked {
			t.Errorf("子类为 0 时成就 %d 不应解锁: %+v", tc.id, got[tc.id])
		}
	}
}

// 装配层：竿子类从 item_def.attrs_json.fopbhlmkcpe 解析，轮子类只认竿上真装的那只。
func TestCastDistanceProfileCarriesRodAndReelType(t *testing.T) {
	srv, _ := newRigTestServer(t)
	ctx := context.Background()
	const uid = int64(1000001)
	sess := newProbeSess(srv)

	bolo, err := srv.CreateItem(ctx, uid, "bolo_10021_LB400", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	if p := srv.castDistanceForRig(ctx, uid, bolo.GUID, 2); p.RodType != 4 || p.ReelType != 0 {
		t.Errorf("博洛尼亚竿应解析为竿型 4、无轮 0，实得 rod=%d reel=%d", p.RodType, p.ReelType)
	}

	casting, err := srv.CreateItem(ctx, uid, "cast_10006_C60MH", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	reel, err := srv.CreateItem(ctx, uid, "bclp_5004", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	line, err := srv.CreateItem(ctx, uid, "mono_40853", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	mountGearForTest(t, srv, sess, casting.GUID, reel.GUID)
	mountGearForTest(t, srv, sess, casting.GUID, line.GUID)
	if p := srv.castDistanceForRig(ctx, uid, casting.GUID, 5); p.RodType != 2 || p.ReelType != 2 {
		t.Errorf("枪柄竿+抛饵卷线器应解析为竿型 2、轮型 2，实得 rod=%d reel=%d", p.RodType, p.ReelType)
	}

	// 竿 GUID 无效时 rigGearIDs 会回退到背包里的任意一支竿，轮子解析因此可能
	// 落到无关装备上；RodParsed 为 false 就是「这一杆的竿没解析出来」的信号，
	// 14/4 装配点据此把竿型/轮型一起置 0，不拿别的竿判成就。
	if p := srv.castDistanceForRig(ctx, uid, "ffffffffffffffffffffffffffffffff", 1); p.RodParsed {
		t.Errorf("无效竿 GUID 不应被当成解析成功，实得 rod=%d parsed=%v", p.RodType, p.RodParsed)
	}
	if p := srv.castDistanceForRig(ctx, uid, "", 1); p.RodParsed || p.RodType != 0 || p.ReelType != 0 {
		t.Errorf("空竿 GUID 应解析为 0/0，实得 rod=%d reel=%d parsed=%v", p.RodType, p.ReelType, p.RodParsed)
	}
}

// 端到端：14/4 结算装配的事件确实带上了当前竿/轮子类，并点亮对应成就。
func TestFishLandedEventCarriesRodAndReelType(t *testing.T) {
	srv, db := newRigTestServer(t)
	ctx := context.Background()
	const uid = int64(1000001)
	sess := newProbeSess(srv)
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	rod, err := srv.CreateItem(ctx, uid, "bolo_10021_LB400", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	st := srv.stateFor(uid)
	st.mu.Lock()
	st.GearGuid = rod.GUID
	st.Bait = "worm_01"
	st.Pending = &RolledFish{FishGUID: "44444444444444444444444444444444", Species: "perch", Grade: 2,
		WeightG: 642, LengthM: .28, LocLevel: "level_001_lake"}
	st.CastActive, st.Hooked = true, true
	st.mu.Unlock()

	body, handled := srv.fishingEndSettle(&Ctx{Sess: sess, CallID: 1, Main: 14, Sub: 4,
		ExpectResponse: true, Log: logging.Discard()})
	if !handled || len(body) == 0 {
		t.Fatal("14/4 结算未返回")
	}
	states, err := db.PlayerAchievementStates(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if !states[102].Unlocked || !states[102].Claimed {
		t.Errorf("博洛尼亚竿上鱼后成就 102 未解锁: %+v", states[102])
	}
	for _, id := range []int64{104, 156, 157, 204, 209} {
		if states[id].Unlocked {
			t.Errorf("博洛尼亚竿上鱼不应解锁成就 %d: %+v", id, states[id])
		}
	}
}
