package gameserver

import (
	"context"
	"strings"
	"testing"
	"time"

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
