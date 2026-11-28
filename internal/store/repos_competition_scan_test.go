package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	sqlite "modernc.org/sqlite"
)

// competitionMemberCountQuery 是 SetCompetitionTeam 判定「是否成员」的查询片段。
const competitionMemberCountQuery = "COUNT(*) FROM competition_member WHERE comp_id=? AND uid=?"

// faultConnector 在真实 SQLite 连接外再包一层：只有包含 match 的查询返回错误，
// 用来构造「数据库读取故障」，与「行不存在（sql.ErrNoRows）」区分开。
type faultConnector struct {
	base  driver.Connector
	match string
	fault error
}

func (c *faultConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &faultConn{Conn: conn, match: c.match, fault: c.fault}, nil
}

func (c *faultConnector) Driver() driver.Driver { return c.base.Driver() }

// faultConn 只实现 QueryContext；其余能力沿用底层连接（走 Prepare 回退路径）。
type faultConn struct {
	driver.Conn
	match string
	fault error
}

func (c *faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, c.match) {
		return nil, c.fault
	}
	if queryer, ok := c.Conn.(driver.QueryerContext); ok {
		return queryer.QueryContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

// openCompetitionFaultDB 打开一个已迁移、且命中 match 的查询必然失败的库。
func openCompetitionFaultDB(t *testing.T, match string, fault error) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "social.db")
	seed, err := Open(path)
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	if err := seed.Migrate(); err != nil {
		t.Fatalf("迁移测试库: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("关闭测试库: %v", err)
	}
	base, err := sqlite.NewConnector("file:" + path)
	if err != nil {
		t.Fatalf("创建 SQLite 连接器: %v", err)
	}
	sqlDB := sql.OpenDB(&faultConnector{base: base, match: match, fault: fault})
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return &DB{SQL: sqlDB, Path: path}
}

// TestSetCompetitionTeamQueryFaultIsNotMemberError 钉住「成员计数查询失败被吞」：
// 查询出错时必须把真实错误交给调用方，不能把 exists 留在 0 后返回
// ErrCompetitionNotMember——否则数据库故障会被显示成「你不是成员」。
func TestSetCompetitionTeamQueryFaultIsNotMemberError(t *testing.T) {
	ctx := context.Background()
	fault := errors.New("注入：读取竞赛成员失败")
	db := openCompetitionFaultDB(t, competitionMemberCountQuery, fault)
	comp := &Competition{ID: "comp-fault", Name: "Cup", State: CompetitionCreated, Owner: 1,
		MaxMembers: 4, CondJSON: `{"win_cond":2}`, DurationSec: 3600}
	if err := db.CreateCompetition(ctx, comp, false); err != nil {
		t.Fatalf("建赛失败: %v", err)
	}
	// uid=2 未报名，UPDATE 影响 0 行，随后进入成员计数分支。
	_, _, err := db.SetCompetitionTeam(ctx, comp.ID, 2, 0)
	if err == nil {
		t.Fatal("成员计数查询失败时 SetCompetitionTeam 应返回错误，实得 nil（故障被吞）")
	}
	if errors.Is(err, ErrCompetitionNotMember) {
		t.Fatalf("数据库故障被伪装成 ErrCompetitionNotMember，期望上抛原始错误: %v", err)
	}
	if !errors.Is(err, fault) {
		t.Fatalf("应上抛原始查询错误 %v，实得 %v", fault, err)
	}
}

// TestSetCompetitionTeamNonMemberStillRejected 是同一分支的回归：查询正常、计数为 0
// 时仍返回 ErrCompetitionNotMember，保证修复没有把「真的不是成员」也变成原始错误。
func TestSetCompetitionTeamNonMemberStillRejected(t *testing.T) {
	ctx := context.Background()
	db := openSocialTestDB(t)
	comp := &Competition{ID: "comp-nonmember", Name: "Cup", State: CompetitionCreated, Owner: 1,
		MaxMembers: 4, CondJSON: `{"win_cond":2}`, DurationSec: 3600}
	if err := db.CreateCompetition(ctx, comp, false); err != nil {
		t.Fatalf("建赛失败: %v", err)
	}
	_, _, err := db.SetCompetitionTeam(ctx, comp.ID, 2, 0)
	if !errors.Is(err, ErrCompetitionNotMember) {
		t.Fatalf("非成员改队伍应返回 ErrCompetitionNotMember，实得 %v", err)
	}
}
