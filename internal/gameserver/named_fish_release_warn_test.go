package gameserver

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"rf4server/internal/proto"
	"rf4server/internal/store"
)

// TestFishingReleaseWarnsWhenNamedFishRecatchFails 锁住 14/6 放生路径对
// NamedFishNoteRecatch 返回值的检查：计数失败只告警，不回滚已经完成的放生。
//
// 用 BEFORE UPDATE 触发器只让命名鱼计数写失败，放生回执事务本身照常提交；
// 回退成忽略返回值时这条测试会因为缺少 warn 而失败。
func TestFishingReleaseWarnsWhenNamedFishRecatchFails(t *testing.T) {
	s, db := newRigTestServer(t)
	sess := newProbeSess(s)
	uid := sess.Userid
	bg := context.Background()

	if _, err := db.EnsureProfile(bg, uid); err != nil {
		t.Fatal(err)
	}
	const namedID = "named-release-warn"
	if err := db.NamedFishInsert(bg, &store.NamedFish{
		FishID: namedID, Species: "perch", Name: "老兵", Grade: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`CREATE TRIGGER fail_named_recatch BEFORE UPDATE ON named_fish
BEGIN
  SELECT RAISE(ABORT, '模拟命名鱼计数写失败');
END`); err != nil {
		t.Fatal(err)
	}

	fish := &RolledFish{
		FishGUID: strings.Repeat("a", 32), Species: "perch", Grade: 4,
		XP: 10, LocLevel: "level_019_american_pond", NamedID: namedID,
	}
	st := s.stateFor(uid)
	st.mu.Lock()
	st.Pending = fish
	st.Hooked = true
	st.mu.Unlock()

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tree := proto.Args524(proto.Guid(strings.Repeat("0", 32)), proto.Guid(fish.FishGUID))
	c := &Ctx{
		Sess: sess, CallID: 73, Main: 14, Sub: 6, Tree: tree,
		DecodeOK: true, ExpectResponse: true, Log: log,
	}
	body, handled := s.fishingRelease(c)
	if !handled || len(body) == 0 {
		t.Fatalf("命名鱼计数失败不应阻断放生主流程，handled=%v body=%x", handled, body)
	}
	if !strings.Contains(logs.String(), "命名鱼重复钓获计数失败") {
		t.Fatalf("命名鱼计数失败必须留下 warn，实际日志: %q", logs.String())
	}
	got, err := db.NamedFishGet(bg, namedID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Catches != 0 {
		t.Fatalf("触发器中止后 catches 应保持 0，实际 %d", got.Catches)
	}
}
