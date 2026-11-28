package gameserver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"rf4server/internal/logging"
	"rf4server/internal/proto"
	"rf4server/internal/store"
)

// 911「大放水! 快逃吧!」= 释放 10 条稀有鱼。稀有沿用全仓口径
// grade>=recordTrophyGrade（星及以上）；放生不改 catch_log.source，所以计数由
// releaseCatchOnce 在放生事务里写 player_counters 的 rare_released。

// release911 走一次完整放生：14/4 结算 → 14/6 放生，返回带 afterResponse 的上下文。
func release911(t *testing.T, s *Server, sess *Session, fish *RolledFish, call int64) *Ctx {
	t.Helper()
	st := s.stateFor(sess.Userid)
	st.mu.Lock()
	st.Pending = fish
	st.Hooked = true
	st.mu.Unlock()
	if body, handled := s.fishingEndSettle(&Ctx{Sess: sess, CallID: uint32(call * 2), Main: 14, Sub: 4,
		ExpectResponse: true, Log: logging.Discard()}); !handled || len(body) == 0 {
		t.Fatal("14/4 结算未正常返回")
	}
	tree := proto.Args524(proto.Guid(strings.Repeat("0", 32)), proto.Guid(fish.FishGUID))
	c := &Ctx{Sess: sess, CallID: uint32(call*2 + 1), Main: 14, Sub: 6,
		Tree: tree, DecodeOK: true, ExpectResponse: true, Log: logging.Discard()}
	if body, handled := s.fishingRelease(c); !handled || len(body) == 0 {
		t.Fatal("14/6 放生未正常返回")
	}
	return c
}

// rareFish911 造一尾指定档位的鱼，GUID 由 seq 决定，保证不同鱼互不重放。
func rareFish911(seq int, grade int64) *RolledFish {
	return &RolledFish{FishGUID: fmt.Sprintf("%032d", seq), Species: "perch", Grade: grade,
		WeightG: 2000, LengthM: 0.5, LocLevel: "level_001_lake"}
}

// achievement911 读 911 的解锁状态；未解锁时表里没有这一行，返回零值。
func achievement911(states map[int64]store.AchievementState) store.AchievementState {
	return states[911]
}

func TestRareReleasedCountOnlyCountsRareReleases(t *testing.T) {
	t.Parallel()
	s, db := newRigTestServer(t)
	sess := newProbeSess(s)
	uid := sess.Userid
	ctx := context.Background()
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if err := db.TunableSet(ctx, "level_gifts_enabled", "0", "test"); err != nil {
		t.Fatal(err)
	}

	// 3 条稀有鱼放生，普通鱼（grade 2）不该计入。
	for i := 1; i <= 3; i++ {
		release911(t, s, sess, rareFish911(i, recordTrophyGrade), int64(i))
	}
	release911(t, s, sess, rareFish911(50, 2), 10)
	got, err := s.rareReleasedCount(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Fatalf("稀有鱼放生计数 = %d，期望 3（grade<3 的放生不该计入）", got)
	}

	// 入护不是放生：计数器不该动。
	kept := rareFish911(60, superTrophyGrade)
	st := s.stateFor(uid)
	st.mu.Lock()
	st.Pending = kept
	st.Hooked = true
	st.mu.Unlock()
	if body, handled := s.fishingEndSettle(&Ctx{Sess: sess, CallID: 900, Main: 14, Sub: 4,
		ExpectResponse: true, Log: logging.Discard()}); !handled || len(body) == 0 {
		t.Fatal("入护前结算未正常返回")
	}
	keepTree := proto.Args524(proto.Guid(strings.Repeat("0", 32)), proto.Guid(kept.FishGUID))
	if body, handled := s.fishingKeep(&Ctx{Sess: sess, CallID: 901, Main: 14, Sub: 5,
		Tree: keepTree, DecodeOK: true, ExpectResponse: true, Log: logging.Discard()}); !handled || len(body) == 0 {
		t.Fatal("14/5 入护未正常返回")
	}
	got, err = s.rareReleasedCount(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Fatalf("入护后稀有鱼放生计数 = %d，期望仍为 3（入护不是放生）", got)
	}

	// 同一尾鱼重放放生（回执命中）：不重复累加。
	if _, _, replayed, err := s.releaseCatchOnce(ctx, uid, fmt.Sprintf("%032d", 1), 10, 1, recordTrophyGrade); err != nil {
		t.Fatal(err)
	} else if !replayed {
		t.Fatal("同一尾鱼再次放生应命中回执、判为重放")
	}
	got, err = s.rareReleasedCount(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Fatalf("重放放生后计数 = %d，期望 3（重放不重复计数）", got)
	}
}

func TestAchievement911NeedsTenRareReleases(t *testing.T) {
	t.Parallel()
	s, db := newRigTestServer(t)
	sess := newProbeSess(s)
	uid := sess.Userid
	ctx := context.Background()
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if err := db.TunableSet(ctx, "level_gifts_enabled", "0", "test"); err != nil {
		t.Fatal(err)
	}

	release := func(i int) *Ctx {
		return release911(t, s, sess, rareFish911(i, recordTrophyGrade), int64(i))
	}
	for i := 1; i <= 9; i++ {
		release(i)
	}
	states, err := db.PlayerAchievementStates(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if st := achievement911(states); st.Unlocked || st.Claimed {
		t.Fatalf("9 条稀有鱼放生不该解锁 911，实得 %+v", st)
	}

	c := release(10)
	if len(c.afterResponse) != 1 {
		t.Errorf("第 10 条稀有鱼放生应排 1 次解锁推送，实得 %d", len(c.afterResponse))
	}
	states, err = db.PlayerAchievementStates(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if st := achievement911(states); !st.Unlocked || !st.Claimed {
		t.Fatalf("累计 10 条稀有鱼放生应解锁 911，实得 %+v", st)
	}

	// 已有 10 条后再放生：已点亮，不再推送，也不重复写状态。
	c = release(11)
	if len(c.afterResponse) != 0 {
		t.Errorf("911 已解锁后再放生应无推送，实得 %d 次", len(c.afterResponse))
	}
	if claimed, err := db.UnlockAndClaimAchievement(ctx, uid, 911, 1, 1); err != nil {
		t.Fatal(err)
	} else if claimed {
		t.Error("911 已点亮后 UnlockAndClaimAchievement 应为幂等 false")
	}
	got, err := s.rareReleasedCount(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if got != 11 {
		t.Errorf("第 11 次放生后计数 = %d，期望 11", got)
	}
}
