package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"rf4server/internal/store"
)

// SQLiteStore 会话凭据与重连接管的本地存储实现（共享 SQLite 文件，WAL 并发）。
// 生产与本地单机同用；MemStore 仅供测试。
type SQLiteStore struct {
	DB  *store.DB
	log *slog.Logger
}

// NewSQLiteStore 构建本地存储。
func NewSQLiteStore(db *store.DB, log *slog.Logger) *SQLiteStore {
	return &SQLiteStore{DB: db, log: log}
}

func (s *SQLiteStore) SaveToken(ctx context.Context, token string, info TokenInfo, ttl time.Duration) error {
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	_, err = s.DB.SQL.ExecContext(ctx, `
		INSERT INTO gw_token(token, info_json, expires) VALUES(?,?,?)
		ON CONFLICT (token) DO UPDATE SET info_json=?, expires=?`,
		token, string(b), time.Now().Add(ttl).Unix(),
		string(b), time.Now().Add(ttl).Unix())
	return err
}

func (s *SQLiteStore) LookupToken(ctx context.Context, token string) (TokenInfo, bool, error) {
	var infoJSON string
	var expiresF float64 // gw_token.expires 列是 REAL，SQLite 读回 float64
	err := s.DB.SQL.QueryRowContext(ctx,
		`SELECT info_json, expires FROM gw_token WHERE token=?`, token).Scan(&infoJSON, &expiresF)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 客户端可能回传 logintoken（登录 XML 里的 token 前 64 字符）——
			// 前缀映射回完整 token 再取
			return s.lookupByPrefix(ctx, token)
		}
		return TokenInfo{}, false, err
	}
	return decodeTokenInfo(infoJSON, expiresF)
}

// lookupByPrefix 前缀匹配（token 以 logintoken 开头）。
func (s *SQLiteStore) lookupByPrefix(ctx context.Context, prefix string) (TokenInfo, bool, error) {
	if len(prefix) < 8 {
		return TokenInfo{}, false, nil
	}
	var infoJSON string
	var expiresF float64
	err := s.DB.SQL.QueryRowContext(ctx,
		`SELECT info_json, expires FROM gw_token WHERE token LIKE ?||'%' AND length(token)>? ORDER BY expires DESC LIMIT 1`,
		prefix, len(prefix)).Scan(&infoJSON, &expiresF)
	if err != nil {
		return TokenInfo{}, false, nil // 无前缀命中
	}
	return decodeTokenInfo(infoJSON, expiresF)
}

func decodeTokenInfo(infoJSON string, expires float64) (TokenInfo, bool, error) {
	if float64(time.Now().Unix()) > expires {
		return TokenInfo{}, false, nil
	}
	var info TokenInfo
	if err := json.Unmarshal([]byte(infoJSON), &info); err != nil {
		return TokenInfo{}, false, err
	}
	return info, true, nil
}

func (s *SQLiteStore) SaveHandoff(ctx context.Context, token string, h Handoff, ttl time.Duration) error {
	h.SavedAt = time.Now().Unix()
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	_, err = s.DB.SQL.ExecContext(ctx, `
		INSERT INTO gw_handoff(token, state_json, saved_at) VALUES(?,?,?)
		ON CONFLICT (token) DO UPDATE SET state_json=?, saved_at=?`,
		token, string(b), h.SavedAt, string(b), h.SavedAt)
	return err
}

func (s *SQLiteStore) LoadHandoff(ctx context.Context, token string) (*Handoff, error) {
	var stateJSON string
	err := s.DB.SQL.QueryRowContext(ctx,
		`SELECT state_json FROM gw_handoff WHERE token=?`, token).Scan(&stateJSON)
	if errors.Is(err, sql.ErrNoRows) {
		// 「没有可承接状态」是正常分支，用 nil, nil 表达。
		return nil, nil
	}
	if err != nil {
		// 读库真出错（库已关闭、锁冲突）必须报出去：把它当成「没有 handoff」
		// 会让重连静默退回新密码流，日志里只留下满屏「信封解析失败」。
		return nil, err
	}
	var h Handoff
	if err := json.Unmarshal([]byte(stateJSON), &h); err != nil {
		return nil, err
	}
	return &h, nil
}

func (s *SQLiteStore) DeleteHandoff(ctx context.Context, token string) error {
	_, err := s.DB.SQL.ExecContext(ctx, `DELETE FROM gw_handoff WHERE token=?`, token)
	return err
}

// useridSeqKey 共享 userid 计数器在 kv 表中的键。
const useridSeqKey = "userid_seq"

// NextUserid 分配下一个全局 userid（登录签发用）。
//
// 取当前值与写回新值必须是同一条语句：先读后写会让并发调用读到同一个旧值，
// 两名玩家拿到同一个 userid，进而共用一份档案与背包。这里用 UPSERT 自增，
// 写事务由 SQLite 串行化，分配结果直接取自 RETURNING。
func (s *SQLiteStore) NextUserid(ctx context.Context) (int64, error) {
	var next int64
	err := s.DB.SQL.QueryRowContext(ctx, `
		INSERT INTO kv(k, v) VALUES(?, '1')
		ON CONFLICT (k) DO UPDATE SET v = CAST(CAST(v AS INTEGER) + 1 AS TEXT)
		WHERE v GLOB '[0-9]*' AND v NOT GLOB '*[^0-9]*'
		RETURNING CAST(v AS INTEGER)`, useridSeqKey).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		// 既有值不是十进制整数时不执行自增：CAST 会把异常值当成 0，写回即把
		// 计数器重置回 1，新账号会与既有玩家撞号。这种情况报错并保留原值。
		if v, ok, gerr := s.DB.KVGet(ctx, useridSeqKey); gerr == nil && ok {
			return 0, fmt.Errorf("userid 计数器 %q 不是十进制整数，拒绝重置", v)
		}
	}
	if err != nil {
		// 读库真出错必须报出去：当成「没有计数器」会静默把序号打回 1。
		return 0, fmt.Errorf("分配 userid 序号: %w", err)
	}
	if next <= 0 {
		return 0, fmt.Errorf("分配 userid 序号: 得到非法值 %d", next)
	}
	return next, nil
}
