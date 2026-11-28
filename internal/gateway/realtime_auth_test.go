package gateway_test

import (
	"encoding/binary"
	"io"
	"net"
	"regexp"
	"testing"
	"time"

	"rf4server/internal/proto"
	"rf4server/internal/testutil"
)

// authPacket 组认证包：01 <重连标记> + u32 长度 + token。
func authPacket(token string, reconnect bool) []byte {
	flag := byte(0x00)
	if reconnect {
		flag = 0x01
	}
	pkt := append([]byte{proto.AuthMagic, flag}, binary.LittleEndian.AppendUint32(nil, uint32(len(token)))...)
	return append(pkt, token...)
}

// dialAndReadGreeting 用给定 token 握手，返回是否收到问候以及读到的错误。
func dialAndReadGreeting(t *testing.T, addr, token string, reconnect bool) (greeted bool, readErr error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("连接 realtime 失败: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write(authPacket(token, reconnect)); err != nil {
		t.Fatalf("发送认证包失败: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	hdr := make([]byte, 36)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return false, err
	}
	if string(hdr) != proto.GreetingUUID {
		t.Fatalf("问候值不符: %q", hdr)
	}
	return true, nil
}

// TestGatewayForgedTokenRejected 真实网关上的鉴权绕过回归：
// 手写的四段 token 在格式上完全合法（>=3 个竖线），但从未经登录签发，
// 必须被拒绝并且不建立会话；同一时刻合法登录签发的 token 仍能正常接到问候。
//
// 事故形态：未登记 token 会从第三段解析 uid 后直接放行，任何人只要拼一个
// 形如 a|b|1000001|c 的串就能以 uid=1000001 接入，绕开 gw_token 的签发校验。
func TestGatewayForgedTokenRejected(t *testing.T) {
	stub, addr := testutil.StartStubGameServer(t, testutil.TestPSK)
	gw := testutil.StartGateway(t, addr, testutil.TestPSK)

	forged := "a|b|1000001|c"
	greeted, readErr := dialAndReadGreeting(t, gw.RealAddr, forged, false)
	if greeted {
		t.Fatalf("伪造 token %q 收到问候：任何人都能凭格式合法的串冒用 uid 接入 realtime", forged)
	}
	if ne, ok := readErr.(net.Error); ok && ne.Timeout() {
		t.Fatal("伪造 token 的连接既没有放行也没有关闭（读超时），鉴权分支没有拒绝它")
	}

	// 没有建立会话就不会有隧道 OPEN。
	if opened, _, _ := stub.Stats(); len(opened) != 0 {
		t.Fatalf("伪造 token 让主服收到 %d 条 OPEN：会话已经建立，鉴权被绕过", len(opened))
	}

	// 回归：同一网关签发的合法 token 必须照常完成握手。
	token, _ := gw.Login(t)
	greeted, readErr = dialAndReadGreeting(t, gw.RealAddr, token, false)
	if !greeted {
		t.Fatalf("合法 token 没能完成握手（err=%v）：修复把正常登录一起拒了", readErr)
	}
}

// TestGatewayLoginIssuedCredentialsAuthenticate 锁住 logintoken 的可用性。
//
// 未登记 token 一律拒绝之后，客户端若把登录 XML 里的 <logintoken> 当作 realtime
// 凭据回传，这条路只能靠「签发时一并登记」支撑；漏登记就等于把这类客户端全拒掉。
func TestGatewayLoginIssuedCredentialsAuthenticate(t *testing.T) {
	_, addr := testutil.StartStubGameServer(t, testutil.TestPSK)
	gw := testutil.StartGateway(t, addr, testutil.TestPSK)

	gw.Login(t)
	m := regexp.MustCompile(`<logintoken>([^<]+)</logintoken>`).FindStringSubmatch(gw.LoginDisplay)
	if m == nil {
		t.Fatalf("登录响应里没有 logintoken: %s", gw.LoginDisplay)
	}
	logintoken := m[1]

	greeted, readErr := dialAndReadGreeting(t, gw.RealAddr, logintoken, false)
	if !greeted {
		t.Fatalf("登录签发的 logintoken 被拒绝（err=%v）：登录 XML 给的凭据必须能接 realtime", readErr)
	}
}
