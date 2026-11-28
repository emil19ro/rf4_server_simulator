package gameserver

import (
	"context"
	"strings"
	"sync"
	"testing"

	"log/slog"

	"rf4server/internal/config"
	"rf4server/internal/store"
)

// catchLogSink 收集 WARN 日志，用来断言「非 ErrNoRows 的钩子查询失败」没有静默。
type catchLogSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *catchLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *catchLogSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// newCatchLogHookSrv 构建记录 WARN 的测试服，用来同时断言落库结果与告警日志。
func newCatchLogHookSrv(t *testing.T) (*Server, *store.DB, *catchLogSink) {
	t.Helper()
	db := openMigratedTestDB(t)
	sink := &catchLogSink{}
	log := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelWarn}))
	srv := NewServer(config.Game{Tunnel: config.Tunnel{PSK: "hook"}}, log, nil)
	srv.SetDB(db)
	return srv, db, sink
}

// catchLogHook 读取本尾鱼在 catch_log 里落下的 hook 字段。
func catchLogHook(t *testing.T, db *store.DB, fishGUID string) string {
	t.Helper()
	var hook string
	if err := db.SQL.QueryRow(`SELECT hook FROM catch_log WHERE catch_guid=?`, fishGUID).Scan(&hook); err != nil {
		t.Fatalf("读取 catch_log.hook(catch_guid=%s): %v", fishGUID, err)
	}
	return hook
}

// TestCatchLogHookMissingRowLeavesEmpty 钩子行确实不存在时查询返回 sql.ErrNoRows，
// 这是合法情况：结算照常成功，hook 落空串。
func TestCatchLogHookMissingRowLeavesEmpty(t *testing.T) {
	srv, db, sink := newCatchLogHookSrv(t)
	ctx := context.Background()
	const uid = int64(1000001)
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatalf("建玩家档案: %v", err)
	}
	fish := &RolledFish{FishGUID: newFishGUID(), Species: "perch", Grade: 1, WeightG: 500, LengthM: .28,
		LocLevel: "level_019_american_pond", HookGUID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if r := srv.settleCatchXP(ctx, uid, fish, 1); r == nil {
		t.Fatal("钩子行不存在属于合法情况（sql.ErrNoRows），结算不应失败")
	}
	if hook := catchLogHook(t, db, fish.FishGUID); hook != "" {
		t.Fatalf("钩子行不存在时 catch_log.hook 应为空串，实得 %q", hook)
	}
	if logs := sink.String(); strings.Contains(logs, "钓获记录钩子查询失败") {
		t.Fatalf("sql.ErrNoRows 不应产生告警，实得日志: %s", logs)
	}
}

// TestCatchLogHookScanFaultWarnsAndKeepsSettlement 是对「非 sql.ErrNoRows 错误」的处理口径：
// item_id 为 NULL 时扫描失败，属于真实的查询故障。修法选择「记 warn 后继续」——
// hook 只是统计维度，不该回滚已经写好的整笔结算，但也不能继续静默。
func TestCatchLogHookScanFaultWarnsAndKeepsSettlement(t *testing.T) {
	srv, db, sink := newCatchLogHookSrv(t)
	ctx := context.Background()
	const uid = int64(1000001)
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatalf("建玩家档案: %v", err)
	}
	const hookGUID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := db.SQL.Exec(`INSERT INTO item(guid,uid,item_id) VALUES(?,?,NULL)`, hookGUID, uid); err != nil {
		t.Fatalf("写入 item_id 为 NULL 的钩子行: %v", err)
	}
	fish := &RolledFish{FishGUID: newFishGUID(), Species: "perch", Grade: 1, WeightG: 500, LengthM: .28,
		LocLevel: "level_019_american_pond", HookGUID: hookGUID}
	if r := srv.settleCatchXP(ctx, uid, fish, 1); r == nil {
		t.Fatal("钩子查询出现非 ErrNoRows 错误时应告警后继续，实得整笔结算被回滚")
	}
	if hook := catchLogHook(t, db, fish.FishGUID); hook != "" {
		t.Fatalf("钩子查询失败时 catch_log.hook 应为空串，实得 %q", hook)
	}
	if logs := sink.String(); !strings.Contains(logs, "钓获记录钩子查询失败") {
		t.Fatalf("非 ErrNoRows 的钩子查询失败必须留下 warn 日志，实得: %q", logs)
	}
}

// TestCatchLogHookRecordsItemID 是对照：钩子行正常时 hook 记为钩子的 item_id，
// 保证「留空串」只发生在查不到的路径上，不是恒定空值。
func TestCatchLogHookRecordsItemID(t *testing.T) {
	srv, db, _ := newCatchLogHookSrv(t)
	ctx := context.Background()
	const uid = int64(1000001)
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatalf("建玩家档案: %v", err)
	}
	const hookGUID = "cccccccccccccccccccccccccccccccc"
	if _, err := db.SQL.Exec(`INSERT INTO item(guid,uid,item_id) VALUES(?,?,?)`, hookGUID, uid, "c_hook_20748"); err != nil {
		t.Fatalf("写入钩子行: %v", err)
	}
	fish := &RolledFish{FishGUID: newFishGUID(), Species: "perch", Grade: 1, WeightG: 500, LengthM: .28,
		LocLevel: "level_019_american_pond", HookGUID: hookGUID}
	if r := srv.settleCatchXP(ctx, uid, fish, 1); r == nil {
		t.Fatal("正常钩子行不应导致结算失败")
	}
	if hook := catchLogHook(t, db, fish.FishGUID); hook != "c_hook_20748" {
		t.Fatalf("catch_log.hook 应记录钩子 item_id=%q，实得 %q", "c_hook_20748", hook)
	}
}
