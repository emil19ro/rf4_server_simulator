package gateway

import (
	"context"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"rf4server/internal/config"
	"rf4server/internal/metrics"
	"rf4server/internal/proto"
	"rf4server/internal/store"
)

// buildAuthPacket 组认证包：01 <重连标记> + u32 长度 + token。
func buildAuthPacket(token string) []byte {
	pkt := append([]byte{proto.AuthMagic, 0x00}, proto.PackU32(uint32(len(token)))...)
	return append(pkt, token...)
}

// authFailureCount 返回 gateway_realtime_auth_failures_total 的当前值；未注册时返回 -1。
func authFailureCount(t *testing.T, obs *metrics.Server) float64 {
	t.Helper()
	families, err := obs.Registry().Gather()
	if err != nil {
		t.Fatalf("读取指标失败: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "gateway_realtime_auth_failures_total" {
			continue
		}
		all := mf.GetMetric()
		if len(all) != 1 {
			t.Fatalf("认证失败计数器有 %d 个样本，期望 1", len(all))
		}
		return all[0].GetCounter().GetValue()
	}
	return -1
}

// TestRealtimeRejectsUnregisteredToken 锁死鉴权边界：格式合法但从未签发的 token 必须被拒绝。
//
// 判别点：认证包里的 token 同时是 RC4 密钥，而 proto.validAuthToken 只校验字符集与竖线数量。
// 只要实现肯从 token 里解析 uid，手写 a|b|1000001|c 就能以任意 uid 接入 realtime，
// 完全跳过 gw_token 的签发校验。
//
// 断言三件事：不发问候就断开、不建立任何会话、认证失败指标 +1。
func TestRealtimeRejectsUnregisteredToken(t *testing.T) {
	obs := metrics.NewServer("", "gateway-auth-test")
	log := slog.New(slog.DiscardHandler)
	cfg := config.Gateway{RealtimeAddr: "127.0.0.1:0", MaxConns: 8}
	// 拒绝发生在隧道握手之前，这里指向一个不可达地址即可：一旦走到隧道就说明已经放行。
	tm := NewTunnelManager("127.0.0.1:1", "auth-test-psk", log, nil, nil)
	rt := NewRealtimeServer(cfg, NewMemStore(1000001), log, tm, obs)
	if err := rt.Listen(); err != nil {
		t.Fatalf("监听 realtime 失败: %v", err)
	}
	defer func() { _ = rt.Close() }()
	go func() { _ = rt.Serve() }()

	forged := "a|b|1000001|c"
	if _, _, ok := proto.TryParseAuthPacket(buildAuthPacket(forged)); !ok {
		t.Fatalf("伪造 token %q 不满足认证包格式，测试前提不成立", forged)
	}

	conn, err := net.DialTimeout("tcp", rt.RealAddr(), 3*time.Second)
	if err != nil {
		t.Fatalf("连接 realtime 失败: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write(buildAuthPacket(forged)); err != nil {
		t.Fatalf("发送认证包失败: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, readErr := conn.Read(buf)
	if readErr == nil {
		t.Fatalf("伪造 token 收到 %d 字节应答（%q），期望连接被直接关闭", n, buf[:n])
	}
	if ne, ok := readErr.(net.Error); ok && ne.Timeout() {
		t.Fatal("伪造 token 的连接既没有放行也没有关闭（读超时），鉴权分支没有拒绝它")
	}

	rt.sids.Range(func(_, _ any) bool {
		t.Error("伪造 token 建立了 realtime 会话：鉴权被绕过，客户端可以冒用任意 uid")
		return false
	})

	if got := authFailureCount(t, obs); got != 1 {
		t.Fatalf("认证失败计数 %v，期望 1：拒绝必须计入 gateway_realtime_auth_failures_total", got)
	}
}

// TestSQLiteStoreLookupByLogintokenAndPrefix 覆盖 realtime 鉴权依赖的正规路径：
// 登录签发的 logintoken 能直接命中，客户端回传的 token 前缀能映射回完整 token；
// 未签发的 token 必须一条都查不到。
func TestSQLiteStoreLookupByLogintokenAndPrefix(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "gw-token.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移测试库失败: %v", err)
	}
	s := NewSQLiteStore(db, slog.New(slog.DiscardHandler))
	ctx := context.Background()

	const uid = int64(1000001)
	info := TokenInfo{Userid: uid, IssuedAt: time.Now().Unix()}
	token := NewToken(uid)
	logintoken := NewLogintoken(uid)
	for _, tok := range []string{token, logintoken} {
		if err := s.SaveToken(ctx, tok, info, time.Hour); err != nil {
			t.Fatalf("登记签发凭据失败: %v", err)
		}
	}

	hits := []struct {
		name string
		tok  string
	}{
		{"完整 token", token},
		{"logintoken", logintoken},
		{"token 前缀", token[:64]},
	}
	for _, hit := range hits {
		got, found, err := s.LookupToken(ctx, hit.tok)
		if err != nil {
			t.Fatalf("%s 查询出错: %v", hit.name, err)
		}
		if !found {
			t.Fatalf("%s 查不到签发记录：realtime 会把它当成未登记凭据拒绝", hit.name)
		}
		if got.Userid != uid {
			t.Fatalf("%s 解析出的 uid=%d，期望 %d", hit.name, got.Userid, uid)
		}
	}

	// 伪造凭据：既有四段形态，也有 LIKE 通配符形态（%、_、\）。
	// %|%|1000001|% 是真正可利用的那一个——以 % 开头的通配前缀能命中任意记录。
	forgedTokens := []struct{ name, token string }{
		{"四段伪造", "a|b|1000001|c"},
		{"通配四段", "%|%|1000001|%"},
		{"通配双段", "%|%"},
		{"全通配", "%|%|%"},
		{"纯通配", "%%"},
		{"下划线通配", "_|_|1000001|_"},
		{"通配加转义符", `%_\`},
		{"local 前导通配", "local|%|1000001|%"},
		{"通配后缀", "aaaaaaaa|%"},
		{"前导空段通配", "|%|%|1000001|%"},
		{"长通配前缀", "%%%%%%%|%|1000001|%"},
		{"兼容区全角通配", "％|％|1000001|％"},
		{"兼容区下划线", "＿|＿|1000001|＿"},
	}
	for _, forged := range forgedTokens {
		got, found, err := s.LookupToken(ctx, forged.token)
		if err != nil {
			t.Fatalf("伪造 token %q 查询报错 %v：查询异常与未登记必须区分", forged.name, err)
		}
		if found {
			t.Fatalf("伪造 token %q 命中了 uid=%d 的签发记录：通配符生效，realtime 会放行冒名连接", forged.name, got.Userid)
		}
	}

	// 前缀长度门槛是防枚举的，短前缀必须继续拒绝。
	if _, found, err := s.LookupToken(ctx, token[:7]); err != nil || found {
		t.Fatalf("7 字符前缀查到了记录（found=%v err=%v）：长度门槛失效，可被枚举", found, err)
	}
}

// TestSQLiteStorePrefixQueryIsLiteral 独立锁定查询层的前缀语义：入口 guard 仍开启时，
// 直接调用 SQL 查询也必须把 % 和 _ 当普通字符。若 substr 改回 LIKE ?||'%'，
// 本测试会单独失败，不依赖 realtime 端到端的双层防御。
func TestSQLiteStorePrefixQueryIsLiteral(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "gw-token.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移测试库失败: %v", err)
	}
	s := NewSQLiteStore(db, slog.New(slog.DiscardHandler))
	ctx := context.Background()

	const uid = int64(1000001)
	issued := NewToken(uid)
	if err := s.SaveToken(ctx, issued, TokenInfo{Userid: uid, IssuedAt: time.Now().Unix()}, time.Hour); err != nil {
		t.Fatalf("登记签发 token 失败: %v", err)
	}

	// 前提：入口 guard 对这条伪造前缀仍然生效；这里测的是另一层，不会绕过生产路径。
	const forgedPrefix = "%|%|1000001|%"
	if _, found, err := s.lookupByPrefix(ctx, forgedPrefix); err != nil || found {
		t.Fatalf("入口 guard 应拒绝伪造前缀 %q：found=%v err=%v", forgedPrefix, found, err)
	}

	// LIKE 会命中 NewToken 的 hex|unix|1000001|base64 结构，
	// substr 则只接受真正以该前缀开头的记录。
	got, found, err := s.lookupByLiteralPrefix(ctx, forgedPrefix)
	if err != nil {
		t.Fatalf("字面量前缀查询出错: %v", err)
	}
	if found {
		t.Fatalf("查询层把伪造前缀 %q 当成 LIKE 通配串，命中 uid=%d：substr 保护已退化", forgedPrefix, got.Userid)
	}

	// 对照：真实前缀仍应在查询层命中，避免测试只是在断言“一律查不到”。
	got, found, err = s.lookupByLiteralPrefix(ctx, issued[:64])
	if err != nil {
		t.Fatalf("真实前缀查询出错: %v", err)
	}
	if !found || got.Userid != uid {
		t.Fatalf("真实前缀 %q 应命中 uid=%d：found=%v got=%+v", issued[:64], uid, found, got)
	}
}

// TestSQLiteStoreLookupErrorNotReportedAsNotFound 锁住两条查询路径的 err 语义：
// realtime.go 靠 err 区分「查询异常」与「未登记」。前缀查询若把读库错误当成未命中
// 静默吞掉，线上库损坏时只会表现为「所有玩家突然未登记」，日志里看不到真实原因。
func TestSQLiteStoreLookupErrorNotReportedAsNotFound(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "gw-token.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移测试库失败: %v", err)
	}
	s := NewSQLiteStore(db, slog.New(slog.DiscardHandler))
	if err := db.Close(); err != nil {
		t.Fatalf("关闭测试库失败: %v", err)
	}
	ctx := context.Background()

	// 外层：精确查询失败必须报出去。
	if _, found, err := s.LookupToken(ctx, NewToken(1000001)); err == nil {
		t.Fatal("精确查询：库已关闭却返回 nil 错误，realtime 无法区分故障与未签发")
	} else if found {
		t.Fatal("精确查询：库已关闭却报告命中签发记录")
	}

	// 内层：精确查询未命中后走的前缀查询，读库失败同样必须报出去，
	// 不能退化成「未命中」——那正是伪造前缀和库故障被混为一谈的地方。
	if _, found, err := s.lookupByPrefix(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err == nil {
		t.Fatal("前缀查询：库已关闭却返回 nil 错误，读库异常被当成未命中")
	} else if found {
		t.Fatal("前缀查询：库已关闭却报告命中签发记录")
	}
}

// TestMemStoreLookupPrefixRules 保证 MemStore 与 SQLiteStore 的前缀规则一致：
// 测试用的 MemStore 若比生产存储宽松，鉴权用例会掩盖真实行为。
func TestMemStoreLookupPrefixRules(t *testing.T) {
	m := NewMemStore(1000001)
	ctx := context.Background()
	const uid = int64(1000001)
	token := NewToken(uid)
	if err := m.SaveToken(ctx, token, TokenInfo{Userid: uid}, time.Hour); err != nil {
		t.Fatalf("登记 token 失败: %v", err)
	}
	if _, found, _ := m.LookupToken(ctx, token[:12]); !found {
		t.Fatal("合法前缀在 MemStore 查不到：前缀兜底被挡死")
	}
	if _, found, _ := m.LookupToken(ctx, token[:7]); found {
		t.Fatal("7 字符前缀在 MemStore 命中：长度门槛失效")
	}
	if _, found, _ := m.LookupToken(ctx, "%|%|1000001|%"); found {
		t.Fatal("通配前缀在 MemStore 命中：两套存储对伪造前缀判定不一致")
	}
}
