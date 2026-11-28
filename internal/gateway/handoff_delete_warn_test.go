package gateway_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rf4server/internal/config"
	"rf4server/internal/gateway"
	"rf4server/internal/testutil"
)

// failingDeleteStore 让 DeleteHandoff 返回真实错误，其余方法透传。
//
// 只影响「删掉已被消费的承接状态」这一条路径：读承接状态仍走真存储，
// 重连能正常承接旧密码流，从而走到删除那一步。fail 用 atomic.Bool 是因为
// 重连过程中测试侧置位、网关侧读取，跨 goroutine 访问。
type failingDeleteStore struct {
	gateway.Store
	fail    atomic.Bool
	deletes atomic.Int64 // DeleteHandoff 实际被调用的次数
}

func (s *failingDeleteStore) DeleteHandoff(ctx context.Context, token string) error {
	s.deletes.Add(1)
	if s.fail.Load() {
		return errors.New("模拟删除失败")
	}
	return s.Store.DeleteHandoff(ctx, token)
}

// capturingLog 收集日志文本，供断言「删除失败时有告警」。
type capturingLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (c *capturingLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capturingLog) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// waitLog 轮询等待日志里出现 want 片段（日志由会话 goroutine 写出）。
func waitLog(c *capturingLog, want string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.Contains(c.text(), want) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return strings.Contains(c.text(), want)
}

// TestDeleteHandoffErrorWarnsAndKeepsSession 锁住「删除承接状态失败不得静默」。
//
// 承接状态一旦被读走，库里那份快照就过期了：它带着客户端已经消费过的
// wire_id。删除失败若被丢弃，库里就会留着这份旧快照，客户端下次重连若在
// 写者覆盖它之前读到，next_wire_id 回退，下行 RC4 从第一个字节起失步，
// 整条连接解不开，而日志里没有任何线索。
//
// 断言四件事：会话照常建立并正常回包、删除确实被调用过、日志出现告警、
// 且告警不含 token 原文。
func TestDeleteHandoffErrorWarnsAndKeepsSession(t *testing.T) {
	stub, stubAddr := testutil.StartStubGameServer(t, testutil.TestPSK)
	inner := gateway.NewMemStore(1000001)
	store := &failingDeleteStore{Store: inner}
	sink := &capturingLog{}
	log := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelWarn}))

	cfg := config.Gateway{
		RealtimeAddr:          "127.0.0.1:0",
		MaxConns:              32,
		AckClientFrames:       true,
		TransportKeepaliveSec: 3600,
	}
	tm := gateway.NewTunnelManager(stubAddr, testutil.TestPSK, log, nil, nil)
	rt := gateway.NewRealtimeServer(cfg, store, log, tm, nil)
	if err := rt.Listen(); err != nil {
		t.Fatal(err)
	}
	tm.SetGateway(rt)
	go func() { _ = rt.Serve() }()
	t.Cleanup(func() {
		_ = rt.Close()
		tm.Close()
	})

	// realtime 认证只认已登记的 token（见 handleConn），探针 token 先签发。
	const token = "handoff|delete|1000001|probe"
	if err := inner.SaveToken(context.Background(), token, gateway.TokenInfo{Userid: 1000001}, time.Hour); err != nil {
		t.Fatalf("登记探针 token 失败: %v", err)
	}

	// 第一段连接：跑两轮，把 RC4 流与 wire_id 推进出去，断线后写者落库。
	c := testutil.NewFakeClient(t, rt.RealAddr())
	c.Dial(token, false)
	waitTunnelUp(t, stub)
	for i := uint32(1); i <= 2; i++ {
		c.SendRequest(i, 0x03, 0x03)
		c.WaitResponse(3 * time.Second)
	}
	c.Close()
	if waitHandoff(t, inner, token) == nil {
		t.Fatal("断开后没有承接状态，测试前提不成立")
	}

	// 第二段连接：承接旧流，随后的删除失败。
	store.fail.Store(true)
	c.Dial(token, true)
	c.SendRequest(100, 0x03, 0x03)
	if env := c.WaitResponse(5 * time.Second); env.CallID != 100 {
		t.Fatalf("删除承接状态失败后会话没能正常工作: call=%d", env.CallID)
	}

	if store.deletes.Load() == 0 {
		t.Fatal("重连后没有调用 DeleteHandoff，本用例未覆盖目标分支")
	}
	if !waitLog(sink, "承接状态删除失败", 3*time.Second) {
		t.Fatalf("删除承接状态失败没有告警，日志: %q", sink.text())
	}
	if strings.Contains(sink.text(), token) {
		t.Fatal("告警里出现了 token 原文：应只记录长度")
	}
}
