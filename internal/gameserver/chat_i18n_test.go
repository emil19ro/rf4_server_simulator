package gameserver

import (
	"slices"
	"testing"

	"rf4server/internal/config"
	"rf4server/internal/proto"
	"rf4server/internal/store"
)

func TestChatLanguageDefaultsToChinese(t *testing.T) {
	s := &Server{}
	if s.langEN() || s.tr("中", "en") != "中" {
		t.Fatal("未配置语言时必须保持中文（旧行为）")
	}
	s.cfg.Language = config.LanguageEN
	if !s.langEN() || s.tr("中", "en") != "en" {
		t.Fatal("language=en 时应取英文")
	}
}

func TestSpeciesChatNameEnglishWithFallback(t *testing.T) {
	s, db := newRigTestServer(t)
	t.Cleanup(func() { stopFishingRecoveryTimers(s) })
	if _, err := db.SQL.Exec(`INSERT INTO loc_wiki_fish(loc,fish_key,fish_zh)
		VALUES('i18n-test','perch','河鲈'),('i18n-test','i18n-only-zh','测试鱼')`); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if got := s.speciesChatName(ctx, "perch"); got != "河鲈" {
		t.Fatalf("中文服 perch=%q，期望 河鲈", got)
	}
	s.cfg.Language = config.LanguageEN
	for key, want := range map[string]string{
		"perch":        "Perch",
		"vjun":         "Loach",
		"e.pikeperch":  "Zander",
		"c.carp":       "Crucian Carp",
		"crucian":      "Gibel Carp",
		"i18n-only-zh": "测试鱼",          // 无英文名回退中文
		"i18n-nowhere": "i18n-nowhere", // 都没有回退 key
	} {
		if got := s.speciesChatName(ctx, key); got != want {
			t.Fatalf("英文服 %s=%q，期望 %q", key, got, want)
		}
	}
}

func TestFishChatTextsEnglish(t *testing.T) {
	s, _ := newRigTestServer(t)
	t.Cleanup(func() { stopFishingRecoveryTimers(s) })
	s.cfg.Language = config.LanguageEN
	trophy := &RolledFish{Species: "perch", WeightG: 1234, Grade: 3}
	if got := s.fishLandedText(trophy, true); got != "Caught trophy: Perch 1.23kg (new record)" {
		t.Fatalf("landed=%q", got)
	}
	if got := s.fishReleasedText(trophy); got != "Released trophy: Perch 1.23kg" {
		t.Fatalf("released trophy=%q", got)
	}
	if got := s.fishReleasedText(&RolledFish{Species: "perch", WeightG: 567, Grade: 1}); got != "Released: Perch 567g" {
		t.Fatalf("released=%q", got)
	}
	if got := s.fishLandedText(&RolledFish{Species: "perch", WeightG: 2000, Grade: 4}, false); got != "Caught unique trophy: Perch 2.00kg" {
		t.Fatalf("landed unique=%q", got)
	}
}

func TestOtherChatTextsEnglish(t *testing.T) {
	kept := []*store.KeepnetEntry{{WeightG: 2794}, {WeightG: 2178}, {WeightG: 627}}
	if got := multiHookKeepChatEN(kept); got != "Multi-hook: 3 more fish kept, 5.60 kg total" {
		t.Fatalf("multi-hook=%q", got)
	}
	if got := qualificationCupsBroadcastTextEN(nil); got != "" {
		t.Fatalf("空奖杯应不播报：%q", got)
	}
	names := []string{
		qualificationCupByBranch[1].chatName(true),
		qualificationTotalCup.chatName(true),
	}
	if got := qualificationCupsBroadcastTextEN(names); got != `Rewards received: "Spinning Master", "Grand Master"` {
		t.Fatalf("cups=%q", got)
	}
	if got := qualificationCupByBranch[1].chatName(false); got != "纺车竿高手" {
		t.Fatalf("中文奖杯名=%q", got)
	}
}

func TestHookedChatEnglish(t *testing.T) {
	s, db := newRigTestServer(t)
	t.Cleanup(func() { stopFishingRecoveryTimers(s) })
	s.cfg.Language = config.LanguageEN
	setWeatherTestTunable(t, s, "fish_hooked_chat", "1")
	sess, peer := newTunnelProbe(t, s)
	ch := collectHookedChat(peer)
	rod, err := s.CreateItem(t.Context(), sess.Userid, "tele_10180_4", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.BindShortcut(t.Context(), sess.Userid, 3, rod.GUID); err != nil {
		t.Fatal(err)
	}
	f := &RolledFish{FishGUID: newFishGUID(), GearGUID: rod.GUID, Species: "perch", WeightG: 1234, Grade: 2}
	st := s.selectFishingRod(sess.Userid, rod.GUID)
	st.mu.Lock()
	st.Pending, st.CastActive, st.InWater = f, true, true
	st.mu.Unlock()
	hFishingHooked(miscCtx(sess, 14, 11, rigRequestTree(t, proto.Guid(rod.GUID), proto.Guid(f.FishGUID))))
	want := []string{"[Hooked] Rod #3: Perch 1.234kg"}
	if got := finishHookedChat(t, sess, ch); !slices.Equal(got, want) {
		t.Fatalf("chat = %q, want %q", got, want)
	}
}
