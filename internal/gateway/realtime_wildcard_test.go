package gateway_test

import (
	"net"
	"path/filepath"
	"testing"

	"rf4server/internal/gateway"
	"rf4server/internal/logging"
	"rf4server/internal/proto"
	"rf4server/internal/store"
	"rf4server/internal/testutil"
)

// forgedWildcardTokens 是 P1 点名的通配符伪造形态。
//
// 事故形态：前缀查询写成 token LIKE ?||'%'，而 token 是客户端输入，
// proto.validAuthToken 只校验可打印 ASCII 与竖线数量，挡不住 % _ \。
// 于是 %|%|1000001|% 以 % 通配命中任意已签发的 logintoken/token，
// 绕过签发校验、以记录里的 uid 接入 realtime。
var forgedWildcardTokens = []string{
	"%|%|1000001|%",
	"%|%",
	"%|%|%",
	"%%",
	"_|_|1000001|_",
	`%_\`,
	"local|%|1000001|%",
	"aaaaaaaa|%",
}

// TestGatewayRejectsWildcardToken 端到端锁死通配符伪造：真实网关 + 真实 SQLite 存储，
// 从 realtime 认证入口一路走到 gw_token 前缀查询。
//
// 只对能通过认证包格式校验的形态拨号：其余形态在 proto.TryParseAuthPacket
// 这一层就被挡下，永远到不了查询（同函数下方逐个断言）。
//
// 失败判别：修复前 LIKE 会命中并回问候（dialAndReadGreeting 返回 true），
// 主服也会收到隧道 OPEN。
func TestGatewayRejectsWildcardToken(t *testing.T) {
	stub, addr := testutil.StartStubGameServer(t, testutil.TestPSK)
	db, err := store.Open(filepath.Join(t.TempDir(), "gw-token.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移测试库失败: %v", err)
	}
	gw := testutil.StartGatewayWithStore(t, addr, testutil.TestPSK, gateway.NewSQLiteStore(db, logging.Discard()))

	// 先签发一条真实凭据：攻击的前提是库里已有记录可供通配命中。
	token, _ := gw.Login(t)

	for _, tok := range forgedWildcardTokens {
		if _, _, ok := proto.TryParseAuthPacket(authPacket(tok, false)); !ok {
			// 连认证包格式都过不了的形态到不了查询层，格式层本身就是一道闸。
			continue
		}
		greeted, readErr := dialAndReadGreeting(t, gw.RealAddr, tok, false)
		if greeted {
			t.Fatalf("伪造 token %q 收到问候：通配前缀命中了已签发记录，攻击者可冒用 uid", tok)
		}
		if ne, ok := readErr.(net.Error); ok && ne.Timeout() {
			t.Fatalf("伪造 token %q 既没放行也没关闭（读超时），鉴权分支没有拒绝它", tok)
		}
	}
	if opened, _, _ := stub.Stats(); len(opened) != 0 {
		t.Fatalf("伪造 token 让主服收到 %d 条 OPEN：会话已建立，鉴权被绕过", len(opened))
	}

	// 回归：同一网关签发的合法 token 必须照常完成握手。
	if greeted, readErr := dialAndReadGreeting(t, gw.RealAddr, token, false); !greeted {
		t.Fatalf("合法 token 握手失败（err=%v）：修复把正常登录一起拒了", readErr)
	}
}

// TestAuthPacketFormatRejectsWildcardShapes 补齐格式层的账：P1 列出的伪造形态里，
// 只有 >=3 个竖线或 32/48 位十六进制的串能进入认证流程，其余在解析阶段就被丢弃。
// 这层不替代查询层的字面前缀比较，只是说明另一批形态同样到不了 gw_token。
func TestAuthPacketFormatRejectsWildcardShapes(t *testing.T) {
	for _, tok := range []string{"%|%", "%|%|%", "%%", `%_\`, "aaaaaaaa|%"} {
		if _, _, ok := proto.TryParseAuthPacket(authPacket(tok, false)); ok {
			t.Fatalf("伪造 token %q 通过了认证包格式校验，会进入 gw_token 查询：格式层没有拦住", tok)
		}
	}
	// 对照：真正能被利用的那一个确实通过了格式校验，必须由查询层挡住。
	for _, tok := range []string{"%|%|1000001|%", "_|_|1000001|_", "local|%|1000001|%"} {
		if _, _, ok := proto.TryParseAuthPacket(authPacket(tok, false)); !ok {
			t.Fatalf("伪造 token %q 应通过格式校验（测试前提），实际被拒：判别点转移", tok)
		}
	}
}
