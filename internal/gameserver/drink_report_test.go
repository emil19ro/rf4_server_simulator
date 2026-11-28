package gameserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"rf4server/internal/proto"
	"rf4server/internal/store"
)

func TestDrinkReportText(t *testing.T) {
	cases := []struct {
		name      string
		bottles   int
		longest   time.Duration
		want      string
		wantEmpty bool
	}{
		{"one bottle", 1, 92 * time.Second, "[酒兴] 已喝 1 瓶 · 剩余 01分32秒", false},
		{"three bottles longest wins", 3, 9*time.Minute + 5*time.Second, "[酒兴] 已喝 3 瓶 · 剩余 09分05秒", false},
		{"rounds up seconds", 2, 1500 * time.Millisecond, "[酒兴] 已喝 2 瓶 · 剩余 00分02秒", false},
		{"no bottles", 0, 5 * time.Minute, "", true},
		{"expired", 2, 0, "", true},
		{"negative", 1, -time.Second, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := drinkReportText(c.bottles, c.longest)
			if c.wantEmpty {
				if got != "" {
					t.Fatalf("want empty, got %q", got)
				}
				return
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestDrinkReportKey(t *testing.T) {
	if got := drinkReportKey(1000001); got != "gear-assistant:drink-report:1000001" {
		t.Fatalf("key = %q", got)
	}
}

func TestParseGearCommandDrink(t *testing.T) {
	for _, text := range []string{"3", "酒兴", "喝酒", "饮酒", "drink", "Drink", "DRINK"} {
		cmd := parseGearCommand(text)
		if cmd.mode != "cmd" || cmd.feature != "drink" {
			t.Fatalf("%q: got %+v", text, cmd)
		}
	}
	if !strings.Contains(gearAssistantHelp(), "【酒兴播报】") {
		t.Fatal("help menu missing drink feature")
	}
}

func addReportTestBottle(t *testing.T, db *store.DB, uid int64, category, source string, duration time.Duration) {
	t.Helper()
	tx, err := db.SQL.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := store.AddDrinkBonusTx(t.Context(), tx, uid, category, .1, duration, source); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestDrinkBonusStatusesReadOnly(t *testing.T) {
	db := openMigratedTestDB(t)
	ctx := context.Background()
	addReportTestBottle(t, db, 1000001, "spirit", "a", 15*time.Minute)
	addReportTestBottle(t, db, 1000001, "beer", "b", 5*time.Minute)

	statuses, err := db.DrinkBonusStatuses(ctx, 1000001)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 {
		t.Fatalf("got %d statuses, want 2", len(statuses))
	}
	// 只读：调用前后 drink_bonus_clock 不应被写入。
	var clockCount int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM drink_bonus_clock WHERE uid=1000001`).Scan(&clockCount); err != nil {
		t.Fatal(err)
	}
	if clockCount != 0 {
		t.Fatalf("read-only query wrote clock rows: %d", clockCount)
	}

	// 无酒兴的玩家返回空列表。
	empty, err := db.DrinkBonusStatuses(ctx, 999)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty player: %+v %v", empty, err)
	}
}

func TestDrinkReportGatedByFlagAndPush(t *testing.T) {
	env := newDLineTestEnv(t, 1000001)
	// 开关默认关：即便有酒兴也不推。
	if on, err := readKVFlag(t.Context(), env.db.SQL, drinkReportKey(env.sess.Userid)); err != nil || on {
		t.Fatalf("flag must default off: on=%v err=%v", on, err)
	}
	if env.srv.maybePushDrinkReport(env.sess, time.Now()) {
		t.Fatal("report pushed while flag off")
	}

	// 开启开关；此时仍无酒兴，不推。
	if _, err := env.db.SQL.Exec(`INSERT INTO kv(k,v) VALUES(?,? )`, drinkReportKey(env.sess.Userid), "1"); err != nil {
		t.Fatal(err)
	}
	if env.srv.maybePushDrinkReport(env.sess, time.Now()) {
		t.Fatal("report pushed with no active drink")
	}

	// 加一瓶酒兴后，取数与文本应可用（本测试会话无 tunnel，Push 返回 0，
	// 所以这里只验证门控与取数，不要求真的推出去）。
	addReportTestBottle(t, env.db, env.sess.Userid, "spirit", "g", 12*time.Minute)
	statuses, err := env.db.DrinkBonusStatuses(t.Context(), env.sess.Userid)
	if err != nil || len(statuses) != 1 {
		t.Fatalf("statuses: %+v %v", statuses, err)
	}
	if text := drinkReportText(len(statuses), time.Duration(statuses[0].RemainingMillis)*time.Millisecond); !strings.Contains(text, "已喝 1 瓶") {
		t.Fatalf("report text: %q", text)
	}
}

// waitDrinkReportPush 从真实 tunnel 连接读取下一条 24/27 酒兴播报，
// 跳过同连接上的 4/8 应答、2/2 保活和 19/10 邮件通知。
func waitDrinkReportPush(t *testing.T, h *mailTestSession) string {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case env, ok := <-h.envelopes:
			if !ok {
				t.Fatal("测试隧道已关闭")
			}
			if env.Marker != -1 || env.Main != 24 || env.Sub != 27 || env.Expect == nil || *env.Expect != 1 {
				continue
			}
			tree, n, err := proto.DecodeValue(env.Body, 0, h.sess.Table())
			if err != nil || n != len(env.Body) {
				t.Fatalf("酒兴播报不可完整解码: n=%d len=%d err=%v", n, len(env.Body), err)
			}
			if reason := checkPushArgsStrict(24, 27, tree); reason != "" {
				t.Fatalf("酒兴播报合同不符: %s", reason)
			}
			if tree.ArgCount() != 3 || tree.ArgItem(0).Class != "lfbdbaffeai" {
				t.Fatalf("酒兴播报形状不符: %+v", tree)
			}
			message := tree.ArgItem(0).Field(1)
			text := message.StrOr()
			if !strings.HasPrefix(text, "[酒兴]") {
				t.Fatalf("不是酒兴播报: %q", text)
			}
			return text
		case <-timeout:
			t.Fatal("未收到酒兴播报 24/27")
			return ""
		}
	}
}

func TestDrinkReportPushOnItemUseAfterResponse(t *testing.T) {
	const uid int64 = 1000001
	s := newDrinkUseTestServer(t)
	if err := s.DB.KVSet(t.Context(), drinkReportKey(uid), "1"); err != nil {
		t.Fatal(err)
	}
	row := drinkUseTestItem(t, s, "rd_red_vine", 2, 0)
	h := newMailTestSession(t, s, 1, uid)
	tree := proto.Args524(proto.U32(0), proto.Guid(row.GUID))
	body, err := proto.EncodeValue(tree, s.Table())
	if err != nil {
		t.Fatal(err)
	}
	s.handleEnvelopePayload(h.sess,
		proto.BuildRequestEnvelope(31, 4, 8, append(append([]byte(nil), body...), 0)))
	if text := waitDrinkReportPush(t, h); !strings.Contains(text, "已喝 1 瓶") {
		t.Fatalf("喝酒后的播报=%q", text)
	}
}

func TestDrinkReportPushOnPeriodicKeepaliveTick(t *testing.T) {
	const uid int64 = 1000001
	s := newDrinkUseTestServer(t)
	s.cfg.KeepaliveIntervalSec = 10 // 只让首拍触发第 6 拍任务，不等待真实 6 拍。
	if err := s.DB.KVSet(t.Context(), drinkReportKey(uid), "1"); err != nil {
		t.Fatal(err)
	}
	addReportTestBottle(t, s.DB, uid, "spirit", "heartbeat", 12*time.Minute)
	h := newMailTestSession(t, s, 1, uid)
	h.sess.keepaliveTicks = 5
	h.sess.startKeepalive()
	t.Cleanup(func() { h.sess.Close("drink-report-test") })
	if text := waitDrinkReportPush(t, h); !strings.Contains(text, "已喝 1 瓶") {
		t.Fatalf("心跳播报=%q", text)
	}
}

func TestDrinkReportPushWhenMenuEnabled(t *testing.T) {
	const uid int64 = 1000001
	s := newDrinkUseTestServer(t)
	addReportTestBottle(t, s.DB, uid, "spirit", "menu", 12*time.Minute)
	h := newMailTestSession(t, s, 1, uid)
	tree := proto.Args524(proto.U32(uint32(gearAssistantUID)), proto.StrVal("3 开"))
	body, err := proto.EncodeValue(tree, s.Table())
	if err != nil {
		t.Fatal(err)
	}
	s.handleEnvelopePayload(h.sess,
		proto.BuildRequestEnvelope(32, 19, 1, append(append([]byte(nil), body...), 0)))
	if text := waitDrinkReportPush(t, h); !strings.Contains(text, "已喝 1 瓶") {
		t.Fatalf("开启菜单后的播报=%q", text)
	}
	on, err := readKVFlag(t.Context(), s.DB.SQL, drinkReportKey(uid))
	if err != nil || !on {
		t.Fatalf("菜单未保存开关: on=%v err=%v", on, err)
	}
}
