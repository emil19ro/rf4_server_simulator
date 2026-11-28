package gameserver

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	sqlite "modernc.org/sqlite"

	"rf4server/internal/proto"
	"rf4server/internal/store"
)

const rareReleasedCountQuery = "COALESCE(value,0) FROM player_counters"

// rareCountFaultConnector 只让成就 911 的计数 SELECT 失败，放生事务里的
// BumpPlayerCounter 和其它查询仍走真实 SQLite。
type rareCountFaultConnector struct {
	base  driver.Connector
	fault error
}

func (c *rareCountFaultConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &rareCountFaultConn{Conn: conn, fault: c.fault}, nil
}

func (c *rareCountFaultConnector) Driver() driver.Driver { return c.base.Driver() }

type rareCountFaultConn struct {
	driver.Conn
	fault error
}

func (c *rareCountFaultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, rareReleasedCountQuery) {
		return nil, c.fault
	}
	if q, ok := c.Conn.(driver.QueryerContext); ok {
		return q.QueryContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func openRareCountFaultDB(t *testing.T, path string, fault error) *store.DB {
	t.Helper()
	base, err := sqlite.NewConnector("file:" + path)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := sql.OpenDB(&rareCountFaultConnector{base: base, fault: fault})
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return &store.DB{SQL: sqlDB, Path: path}
}

// TestRareReleasedCountFaultSkipsAchievementEvaluation 锁住查询错误的处理口径：
// 预置 9 条后再放生第 10 条稀有鱼，计数写入成功但读取失败时不能把 0 当成未达标，
// 必须告警并跳过本次成就判定；下次放生仍有重新判定的机会。
func TestRareReleasedCountFaultSkipsAchievementEvaluation(t *testing.T) {
	s, baseDB := newRigTestServer(t)
	fault := errors.New("注入：读取 rare_released 失败")
	db := openRareCountFaultDB(t, baseDB.Path, fault)
	s.SetDB(db)

	sess := newProbeSess(s)
	uid := sess.Userid
	bg := context.Background()
	if _, err := db.EnsureProfile(bg, uid); err != nil {
		t.Fatal(err)
	}
	if err := db.TunableSet(bg, "level_gifts_enabled", "0", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO player_counters(uid,key,value) VALUES(?,?,9)`,
		uid, rareReleasedCounterKey); err != nil {
		t.Fatal(err)
	}

	fish := &RolledFish{
		FishGUID: fmt.Sprintf("%032d", 10), Species: "perch", Grade: recordTrophyGrade,
		WeightG: 2000, LengthM: 0.5, LocLevel: "level_001_lake",
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
		Sess: sess, CallID: 91, Main: 14, Sub: 6, Tree: tree,
		DecodeOK: true, ExpectResponse: true, Log: log,
	}
	body, handled := s.fishingRelease(c)
	if !handled || len(body) == 0 {
		t.Fatalf("计数读取失败不应阻断放生，handled=%v body=%x", handled, body)
	}
	if !strings.Contains(logs.String(), "累计放生稀有鱼计数读取失败，跳过成就判定") {
		t.Fatalf("计数读取失败必须留下 warn，实际日志: %q", logs.String())
	}
	if len(c.afterResponse) != 0 {
		t.Fatalf("读取失败时不能按 0 判定，本次不应推送成就解锁，实得 %d 次", len(c.afterResponse))
	}

	var got int64
	if err := baseDB.SQL.QueryRow(`SELECT value FROM player_counters WHERE uid=? AND key=?`,
		uid, rareReleasedCounterKey).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 10 {
		t.Fatalf("第 10 条稀有鱼放生应把计数累加到 10，实得 %d", got)
	}
}
