package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// migrationNamesInDB 统计迁移登记表里每个名字的登记次数。
func migrationNamesInDB(t *testing.T, db *DB) map[string]int {
	t.Helper()
	rows, err := db.SQL.Query(`SELECT name, COUNT(*) FROM schema_migrations GROUP BY name`)
	if err != nil {
		t.Fatalf("读取迁移登记失败: %v", err)
	}
	defer rows.Close()
	names := make(map[string]int)
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			t.Fatalf("扫描迁移登记失败: %v", err)
		}
		names[name] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历迁移登记失败: %v", err)
	}
	return names
}

// TestMigrateConcurrentFirstBootOnFreshDatabase 覆盖同时首次启动的场景：
// gateway、gameserver 与 admin 共享同一个主库，三者启动都会调 Migrate()。
// 检查是否已应用、执行迁移、登记已应用必须在同一临界区里完成；否则两边都读到
// "未应用"，后到者重放没有 IF NOT EXISTS 的 DDL（如 00007 的 CREATE TABLE
// rig_component、00018 的 catch_log 索引）会失败，Migrate() 返回错误，进程退出。
func TestMigrateConcurrentFirstBootOnFreshDatabase(t *testing.T) {
	// 并发首启的失败率取决于调度：句柄越多、轮次越多越稳定复现原实现的
	// "重放别人已跑过的迁移" 失败（UNIQUE 冲突 / already exists）。
	const rounds = 10
	for round := 1; round <= rounds; round++ {
		t.Run(fmt.Sprintf("第%d轮", round), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fresh.db")
			// 多个句柄模拟多个进程：各自独立的连接池，共享同一个库文件。
			const handles = 4
			dbs := make([]*DB, handles)
			for i := range dbs {
				db, err := Open(path)
				if err != nil {
					t.Fatalf("第 %d 轮打开第 %d 个句柄失败: %v", round, i+1, err)
				}
				dbs[i] = db
				defer func() { _ = db.Close() }()
			}
			first := dbs[0]

			start := make(chan struct{})
			errs := make([]error, handles)
			var wg sync.WaitGroup
			for i, db := range dbs {
				wg.Add(1)
				go func(i int, db *DB) {
					defer wg.Done()
					<-start
					errs[i] = db.Migrate()
				}(i, db)
			}
			close(start)
			wg.Wait()

			for i, err := range errs {
				if err != nil {
					t.Fatalf("第 %d 轮第 %d 个句柄并发首启迁移失败，期望 nil，实际 %v", round, i+1, err)
				}
			}
			// 结构必须完整：抽查并发重放时最容易 "already exists" 的对象。
			assertCount(t, first, `SELECT COUNT(*) FROM pragma_table_info('rig_component') WHERE name='ordinal'`, 1)
			assertCount(t, first, `SELECT COUNT(*) FROM pragma_table_info('catch_log') WHERE name='catch_guid'`, 1)
			assertCount(t, first, `SELECT COUNT(*) FROM pragma_table_info('catch_log') WHERE name='tackle_cat'`, 1)
			assertCount(t, first, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='uq_catch_log_fish_identity'`, 1)
			assertCount(t, first, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='membership_order'`, 1)
			assertCount(t, first, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='drink_bonus_effect'`, 1)

			// 每个迁移文件只能登记一次：并发下重复登记会让后续迁移错乱。
			names := migrationNamesInDB(t, first)
			entries, err := migrationsFS.ReadDir("migrations")
			if err != nil {
				t.Fatalf("读取内嵌迁移目录失败: %v", err)
			}
			if len(names) != len(entries) {
				t.Fatalf("迁移登记条数期望 %d（每个迁移文件一条），实际 %d", len(entries), len(names))
			}
			for name, n := range names {
				if n != 1 {
					t.Fatalf("迁移 %s 期望登记 1 次，实际 %d 次", name, n)
				}
			}
			for _, file := range entries {
				full := "migrations/" + file.Name()
				if _, ok := names[full]; !ok {
					t.Fatalf("迁移 %s 未登记，期望登记 1 次，实际 0 次", full)
				}
			}
		})
	}
}

// TestMigrateIsIdempotentOnSameDatabase 验证同一库连续迁移两次都成功：
// 已登记的迁移必须跳过，登记行既不新增也不改动。
func TestMigrateIsIdempotentOnSameDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idempotent.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Migrate(); err != nil {
		t.Fatalf("首次迁移失败，期望 nil，实际 %v", err)
	}
	first := migrationNamesInDB(t, db)
	if err := db.Migrate(); err != nil {
		t.Fatalf("第二次迁移失败，期望 nil，实际 %v", err)
	}
	second := migrationNamesInDB(t, db)

	if len(first) != len(second) {
		t.Fatalf("二次迁移后登记条数期望 %d，实际 %d", len(first), len(second))
	}
	for name, n := range second {
		if n != 1 {
			t.Fatalf("二次迁移后 %s 期望登记 1 次，实际 %d 次", name, n)
		}
		if first[name] != n {
			t.Fatalf("二次迁移后 %s 的登记次数期望 %d，实际 %d", name, first[name], n)
		}
	}
	assertCount(t, db, `SELECT COUNT(*) FROM pragma_table_info('rig_component') WHERE name='ordinal'`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='membership_order'`, 1)
}
