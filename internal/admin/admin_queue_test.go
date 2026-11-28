package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

// startQueueTestServer 启动一个测试用管理后台并返回基地址与登录 token。
func startQueueTestServer(t *testing.T, db *store.DB) (string, string) {
	t.Helper()
	cfg := config.Admin{HTTPAddr: "127.0.0.1:0", JWTSecret: "test"}
	srv := NewServer(cfg, db, logging.Discard(), WebFS())
	if _, err := srv.Start(); err != nil {
		t.Fatalf("启动管理后台: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	base := "http://" + srv.Addr()
	client := &http.Client{Timeout: 5 * time.Second}
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": "admin123"})
	resp, err := client.Post(base+"/api/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("登录请求失败: %v", err)
	}
	defer resp.Body.Close()
	var login struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&login); err != nil || login.Token == "" {
		t.Fatalf("登录应返回 token，实得状态 %d，解析错误 %v", resp.StatusCode, err)
	}
	return base, login.Token
}

// adminPost 以管理 token 发一次 JSON POST，返回状态码与响应体。
func adminPost(t *testing.T, base, token, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", path, err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	return resp.StatusCode, out.String()
}

// countQueue 统计 admin_queue 中 payload 含 needles 全部片段的指令行数。
func countQueue(t *testing.T, db *store.DB, needles ...string) int64 {
	t.Helper()
	q := "SELECT COUNT(*) FROM admin_queue WHERE 1=1"
	args := make([]any, 0, len(needles))
	for _, n := range needles {
		q += " AND payload LIKE '%'||?||'%'"
		args = append(args, n)
	}
	var n int64
	if err := db.SQL.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("查询 admin_queue 失败: %v", err)
	}
	return n
}

// countAudit 统计 audit_log 中指定 action 的审计行数。
func countAudit(t *testing.T, db *store.DB, action string) int64 {
	t.Helper()
	var n int64
	if err := db.SQL.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action=?", action).Scan(&n); err != nil {
		t.Fatalf("查询 audit_log 失败: %v", err)
	}
	return n
}

// seedAdmin 建管理员与玩家档案，并给发物品接口准备一个可发的物品定义。
func seedAdmin(t *testing.T, db *store.DB) int64 {
	t.Helper()
	ctx := context.Background()
	if err := db.EnsureAdmin(ctx, "admin", "admin123"); err != nil {
		t.Fatalf("建管理员失败: %v", err)
	}
	const uid = int64(1000001)
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatalf("建玩家档案失败: %v", err)
	}
	if _, err := db.SQL.Exec(`INSERT OR REPLACE INTO item_def(numeric_id,item_id,category,kind,brand,attrs_json)
		VALUES(9001,'worm_01',27,'bait','','{}')`); err != nil {
		t.Fatalf("seed item_def 失败: %v", err)
	}
	return uid
}

// TestGrantItemQueuesAndAudits 覆盖正常路径：发放物品返回 200，队列里确有 grant_item 指令，
// 审计表里确有 grant_item 行；两者必须同时成功，否则管理员会看到「发放成功」而物品永不送货。
func TestGrantItemQueuesAndAudits(t *testing.T) {
	db := testDB(t)
	seedAdmin(t, db)
	base, token := startQueueTestServer(t, db)

	code, body := adminPost(t, base, token, "/api/players/grant",
		`{"uid":1000001,"item_id":"worm_01","qty":2}`)
	if code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("发放物品应 200 且返回 {ok:true}，实得状态 %d，响应 %s", code, body)
	}
	if n := countQueue(t, db, "grant_item", `"uid":1000001`, "worm_01"); n != 1 {
		t.Fatalf("admin_queue 应有 1 条 grant_item 指令，实得 %d 条", n)
	}
	if n := countAudit(t, db, "grant_item"); n != 1 {
		t.Fatalf("audit_log 应有 1 条 grant_item 审计，实得 %d 条", n)
	}
}

// TestAdminQueueClosedDBReturns5xx 覆盖指令入队失败：数据库关闭后任何写入都失败，
// 发放物品与踢人都必须回 5xx，不能对管理员谎报成功。
func TestAdminQueueClosedDBReturns5xx(t *testing.T) {
	db := testDB(t)
	seedAdmin(t, db)
	base, token := startQueueTestServer(t, db)
	// 登录走完再关库：认证不查库，所有写库路径都会失败。
	if err := db.Close(); err != nil {
		t.Fatalf("关闭数据库失败: %v", err)
	}
	for _, tc := range []struct {
		name, path, body string
	}{
		{"发放物品", "/api/players/grant", `{"uid":1000001,"item_id":"worm_01","qty":1}`},
		{"踢人", "/api/players/kick", `{"uid":1000001}`},
	} {
		code, body := adminPost(t, base, token, tc.path, tc.body)
		if code < 500 || code > 599 {
			t.Fatalf("%s 在指令入队失败时应回 5xx，实得状态 %d，响应 %s", tc.name, code, body)
		}
		if strings.Contains(body, `"ok":true`) {
			t.Fatalf("%s 在指令入队失败时不应返回成功响应，实得 %s", tc.name, body)
		}
	}
}

// TestAdminAuditFailureAbortsQueueWrite 覆盖审计写入失败：审计行写不进去时整笔回滚，
// 接口回 5xx，且队列里不能留下这条指令（否则物品会被消费而操作没有任何审计痕迹）。
func TestAdminAuditFailureAbortsQueueWrite(t *testing.T) {
	db := testDB(t)
	seedAdmin(t, db)
	if _, err := db.SQL.Exec(`CREATE TRIGGER fail_grant_audit BEFORE INSERT ON audit_log
		WHEN NEW.action='grant_item'
		BEGIN SELECT RAISE(ABORT,'审计写入被拒绝'); END`); err != nil {
		t.Fatalf("创建审计触发器失败: %v", err)
	}
	base, token := startQueueTestServer(t, db)

	code, body := adminPost(t, base, token, "/api/players/grant",
		`{"uid":1000001,"item_id":"worm_01","qty":1}`)
	if code < 500 || code > 599 {
		t.Fatalf("审计写入失败时应回 5xx，实得状态 %d，响应 %s", code, body)
	}
	if !strings.Contains(body, "操作未生效") || !strings.Contains(body, "可重试") {
		t.Fatalf("纯队列操作失败时必须说明未生效且可重试，实得 %s", body)
	}
	if n := countQueue(t, db, "grant_item"); n != 0 {
		t.Fatalf("审计失败后 admin_queue 不应留下 grant_item 指令，实得 %d 条", n)
	}
	if n := countAudit(t, db, "grant_item"); n != 0 {
		t.Fatalf("审计写入失败时 audit_log 不应有 grant_item 行，实得 %d 条", n)
	}
}

// TestAdminQueueFailureRollsBackAudit 覆盖指令入队失败：队列写入失败时审计行同样回滚，
// 接口回 5xx，不能出现「审计记了、指令没入队、接口说成功」的半个事务。
func TestAdminQueueFailureRollsBackAudit(t *testing.T) {
	db := testDB(t)
	seedAdmin(t, db)
	if _, err := db.SQL.Exec(`CREATE TRIGGER fail_grant_queue BEFORE INSERT ON admin_queue
		WHEN NEW.payload LIKE '%grant_item%'
		BEGIN SELECT RAISE(ABORT,'指令入队被拒绝'); END`); err != nil {
		t.Fatalf("创建队列触发器失败: %v", err)
	}
	base, token := startQueueTestServer(t, db)

	code, body := adminPost(t, base, token, "/api/players/grant",
		`{"uid":1000001,"item_id":"worm_01","qty":1}`)
	if code < 500 || code > 599 {
		t.Fatalf("指令入队失败时应回 5xx，实得状态 %d，响应 %s", code, body)
	}
	if n := countQueue(t, db, "grant_item"); n != 0 {
		t.Fatalf("admin_queue 不应有 grant_item 指令，实得 %d 条", n)
	}
	if n := countAudit(t, db, "grant_item"); n != 0 {
		t.Fatalf("指令入队失败时 audit_log 应整笔回滚，实得 %d 条 grant_item 审计", n)
	}
}

// TestBusinessWriteFailureResponseExplainsAppliedState 覆盖"业务已经落库、审计/队列
// 随后失败"的场景：响应必须明确告诉管理员业务已生效、不要重复提交，避免重发导致
// 参数被改两次；同时审计与队列仍须整笔回滚。
func TestBusinessWriteFailureResponseExplainsAppliedState(t *testing.T) {
	db := testDB(t)
	seedAdmin(t, db)
	if _, err := db.SQL.Exec(`CREATE TRIGGER fail_tunable_audit BEFORE INSERT ON audit_log
		WHEN NEW.action='tunable'
		BEGIN SELECT RAISE(ABORT,'tunable 审计写入被拒绝'); END`); err != nil {
		t.Fatalf("创建审计触发器失败: %v", err)
	}
	base, token := startQueueTestServer(t, db)

	code, body := adminPost(t, base, token, "/api/tunables",
		`{"key":"bite_probability","value":"0.37"}`)
	if code < 500 || code > 599 {
		t.Fatalf("审计写入失败时应回 5xx，实得状态 %d，响应 %s", code, body)
	}
	if !strings.Contains(body, "业务已生效") || !strings.Contains(body, "请勿重复提交") {
		t.Fatalf("业务已落库时响应必须说明已生效且不要重复提交，实得 %s", body)
	}
	value, ok, err := db.TunableGet(context.Background(), "bite_probability")
	if err != nil || !ok || value != "0.37" {
		t.Fatalf("业务写入应保留，实得 value=%q ok=%v err=%v", value, ok, err)
	}
	if n := countAudit(t, db, "tunable"); n != 0 {
		t.Fatalf("审计失败后 audit_log 不应留下 tunable 行，实得 %d 条", n)
	}
	if n := countQueue(t, db, "reload_tunables"); n != 0 {
		t.Fatalf("审计失败后 admin_queue 不应留下 reload_tunables，实得 %d 条", n)
	}
}
