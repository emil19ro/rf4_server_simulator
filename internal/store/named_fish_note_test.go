package store

import (
	"context"
	"testing"
)

// seedNamedFish 预置一条命名鱼行，供计数用例断言基数。
func seedNamedFish(t *testing.T, db *DB, fishID string) {
	t.Helper()
	if err := db.NamedFishInsert(context.Background(), &NamedFish{
		FishID: fishID, Species: "perch", Name: "老兵", Grade: 4,
	}); err != nil {
		t.Fatalf("预置命名鱼 %s 失败: %v", fishID, err)
	}
}

func TestNamedFishNoteRecatchIncrementsCatches(t *testing.T) {
	db := openIntegrityTestDB(t)
	ctx := context.Background()
	seedNamedFish(t, db, "recatch-ok")
	if err := db.NamedFishNoteRecatch(ctx, "recatch-ok"); err != nil {
		t.Fatalf("首次计数应返回 nil，实际 err=%v", err)
	}
	if err := db.NamedFishNoteRecatch(ctx, "recatch-ok"); err != nil {
		t.Fatalf("再次计数应返回 nil，实际 err=%v", err)
	}
	got, err := db.NamedFishGet(ctx, "recatch-ok")
	if err != nil {
		t.Fatalf("读数失败: %v", err)
	}
	if got.Catches != 2 {
		t.Fatalf("重复钓获计数应累加：catches=%d，期望 2", got.Catches)
	}
}

func TestNamedFishNoteRecatchEmptyIDIsNoop(t *testing.T) {
	db := openIntegrityTestDB(t)
	ctx := context.Background()
	seedNamedFish(t, db, "recatch-empty")
	if err := db.NamedFishNoteRecatch(ctx, ""); err != nil {
		t.Fatalf("空 fishID 是正常空操作，应返回 nil，实际 err=%v", err)
	}
	got, err := db.NamedFishGet(ctx, "recatch-empty")
	if err != nil {
		t.Fatalf("读数失败: %v", err)
	}
	if got.Catches != 0 {
		t.Fatalf("空 fishID 不应产生写入：catches=%d，期望 0", got.Catches)
	}
}

func TestNamedFishNoteRecatchReportsWriteError(t *testing.T) {
	db := openIntegrityTestDB(t)
	ctx := context.Background()
	seedNamedFish(t, db, "recatch-ro")
	if _, err := db.SQL.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatalf("切换只读连接失败: %v", err)
	}
	if err := db.NamedFishNoteRecatch(ctx, "recatch-ro"); err == nil {
		t.Fatal("只读连接下写库必须返回错误，实际 err=nil（计数失败又被静默丢弃）")
	}
	got, err := db.NamedFishGet(ctx, "recatch-ro")
	if err != nil {
		t.Fatalf("读数失败: %v", err)
	}
	if got.Catches != 0 {
		t.Fatalf("写库失败后不应留下计数：catches=%d，期望 0", got.Catches)
	}
}
