package gateway_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"rf4server/internal/gateway"
	"rf4server/internal/store"
)

// newUseridStore 打开一个空的临时主库，返回被测的 SQLite 存储。
// 计数器每次从空表开始，分配结果可以直接按 1、2、3… 断言。
func newUseridStore(t *testing.T) *gateway.SQLiteStore {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "userid.db"))
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移临时库失败: %v", err)
	}
	return gateway.NewSQLiteStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestNextUseridSequential 空计数器首次分配得到 1，之后逐次加一。
func TestNextUseridSequential(t *testing.T) {
	s := newUseridStore(t)
	ctx := context.Background()
	for want := int64(1); want <= 3; want++ {
		got, err := s.NextUserid(ctx)
		if err != nil {
			t.Fatalf("第 %d 次分配报错: %v", want, err)
		}
		if got != want {
			t.Fatalf("第 %d 次分配期望 userid=%d，实际=%d", want, want, got)
		}
	}
	// 计数器按字符串存回 kv，读回的值应当是最后一次分配结果。
	v, ok, err := s.DB.KVGet(ctx, "userid_seq")
	if err != nil || !ok {
		t.Fatalf("读取计数器期望存在且无错，实际 ok=%v err=%v", ok, err)
	}
	if v != "3" {
		t.Fatalf("三次分配后期望计数器为 \"3\"，实际=%q", v)
	}
}

// TestNextUseridConcurrentUnique 并发分配不得重复：读与写分成两步时，
// 两个调用会读到同一个旧值，两名玩家拿到同一个 userid 并共用档案。
func TestNextUseridConcurrentUnique(t *testing.T) {
	s := newUseridStore(t)
	const n = 100
	ids := make([]int64, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = s.NextUserid(context.Background())
		}(i)
	}
	wg.Wait()

	seen := make(map[int64]int, n)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个并发调用报错: %v", i, err)
		}
		if ids[i] <= 0 {
			t.Fatalf("第 %d 个并发调用期望正数 userid，实际=%d", i, ids[i])
		}
		if prev, dup := seen[ids[i]]; dup {
			t.Fatalf("并发分配出现重复 userid=%d（第 %d 与第 %d 个调用），期望 %d 个互不相同的值",
				ids[i], prev, i, n)
		}
		seen[ids[i]] = i
	}
	if len(seen) != n {
		t.Fatalf("并发分配期望 %d 个不同 userid，实际=%d", n, len(seen))
	}
	// 空计数器起步，100 次分配应恰好用满 1..100。
	for want := int64(1); want <= n; want++ {
		if _, ok := seen[want]; !ok {
			t.Fatalf("并发分配期望覆盖 1..%d，缺少 userid=%d", n, want)
		}
	}
}

// TestNextUseridResumes 既有计数器的值继续使用：存的是字符串，读回照样自增。
func TestNextUseridResumes(t *testing.T) {
	s := newUseridStore(t)
	ctx := context.Background()
	if err := s.DB.KVSet(ctx, "userid_seq", "41"); err != nil {
		t.Fatalf("写入初始计数器失败: %v", err)
	}
	got, err := s.NextUserid(ctx)
	if err != nil {
		t.Fatalf("分配 userid 报错: %v", err)
	}
	if got != 42 {
		t.Fatalf("既有计数器为 41 时期望分配 42，实际=%d", got)
	}
	if v, _, err := s.DB.KVGet(ctx, "userid_seq"); err != nil || v != "42" {
		t.Fatalf("分配后期望计数器为 \"42\"，实际=%q err=%v", v, err)
	}
}

// TestNextUseridCorruptCounter 计数器被写成非十进制整数时报错并保留原值。
// 直接 CAST 会把异常值当成 0，再写回就等于把序号重置成 1，新账号会与既有玩家撞号。
func TestNextUseridCorruptCounter(t *testing.T) {
	for _, corrupt := range []string{"abc", "", "-9", "1.5", "0x10"} {
		t.Run(corrupt, func(t *testing.T) {
			s := newUseridStore(t)
			ctx := context.Background()
			if err := s.DB.KVSet(ctx, "userid_seq", corrupt); err != nil {
				t.Fatalf("写入异常计数器失败: %v", err)
			}
			got, err := s.NextUserid(ctx)
			if err == nil {
				t.Fatalf("计数器为 %q 时期望返回错误，实际分配了 userid=%d 且未报错", corrupt, got)
			}
			if got != 0 {
				t.Fatalf("计数器为 %q 时期望不分配 userid，实际=%d", corrupt, got)
			}
			if v, ok, gerr := s.DB.KVGet(ctx, "userid_seq"); gerr != nil || !ok || v != corrupt {
				t.Fatalf("期望异常计数器被原样保留为 %q，实际=%q ok=%v err=%v", corrupt, v, ok, gerr)
			}
		})
	}
}

// TestNextUseridTwoHandlesUnique 两个连接句柄指向同一个库文件时也不得重复：
// 生产里网关与 admin 进程共享同一个主库，计数器是跨句柄的。
func TestNextUseridTwoHandlesUnique(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	first, err := store.Open(path)
	if err != nil {
		t.Fatalf("打开第一个库句柄失败: %v", err)
	}
	defer func() { _ = first.Close() }()
	if err := first.Migrate(); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	second, err := store.Open(path)
	if err != nil {
		t.Fatalf("打开第二个库句柄失败: %v", err)
	}
	defer func() { _ = second.Close() }()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	stores := [2]*gateway.SQLiteStore{gateway.NewSQLiteStore(first, log), gateway.NewSQLiteStore(second, log)}
	const n = 50
	ids := make([]int64, 2*n)
	errs := make([]error, 2*n)
	var wg sync.WaitGroup
	for i := 0; i < 2*n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = stores[i%2].NextUserid(context.Background())
		}(i)
	}
	wg.Wait()

	seen := make(map[int64]int, 2*n)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个并发调用报错: %v", i, err)
		}
		if prev, dup := seen[ids[i]]; dup {
			t.Fatalf("两个句柄并发分配出现重复 userid=%d（第 %d 与第 %d 个调用），期望 %d 个互不相同的值",
				ids[i], prev, i, 2*n)
		}
		seen[ids[i]] = i
	}
	if len(seen) != 2*n {
		t.Fatalf("两个句柄并发分配期望 %d 个不同 userid，实际=%d", 2*n, len(seen))
	}
}

// TestNextUseridReadFailure 库不可用时必须报错，不能当成「没有计数器」从头发号。
func TestNextUseridReadFailure(t *testing.T) {
	s := newUseridStore(t)
	if err := s.DB.Close(); err != nil {
		t.Fatalf("关闭临时库失败: %v", err)
	}
	got, err := s.NextUserid(context.Background())
	if err == nil {
		t.Fatalf("库已关闭时期望返回错误，实际分配了 userid=%d", got)
	}
	if got != 0 {
		t.Fatalf("库已关闭时期望不分配 userid，实际=%d", got)
	}
}
