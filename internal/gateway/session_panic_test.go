package gateway

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"rf4server/internal/config"
	"rf4server/internal/proto"
)

// syncBuffer 并发安全的日志缓冲：服务端多个 goroutine 都会写日志。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// panicLookupStore 让指定 token 的凭据查询 panic，模拟 handler 内的异常。
type panicLookupStore struct {
	Store
	token string
}

func (s *panicLookupStore) LookupToken(ctx context.Context, token string) (TokenInfo, bool, error) {
	if token == s.token {
		panic("模拟 handler 异常")
	}
	return s.Store.LookupToken(ctx, token)
}

// panicSaveStore 让指定 token 的承接状态落库 panic，模拟写者内的异常。
type panicSaveStore struct {
	Store
	token string
}

func (s *panicSaveStore) SaveHandoff(ctx context.Context, token string, h Handoff, ttl time.Duration) error {
	if token == s.token {
		panic("模拟写者异常")
	}
	return s.Store.SaveHandoff(ctx, token, h, ttl)
}

// startRealtimeOnly 装配一个只跑 realtime 接入的服务。
//
// 隧道用一对已握手的连接直接挂在管理器上：会话的 OPEN 必须发得出去，
// 否则 sendOpen 失败会提前断开，测不到会话建立之后的路径。
func startRealtimeOnly(t *testing.T, store Store) (addr string, srv *RealtimeServer, logs *syncBuffer) {
	t.Helper()
	ensureTestMetrics()
	ensureTunnelMetrics()

	logs = &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	serverConn, peer := dialTunnelPair(t, "panic-test-psk")
	t.Cleanup(func() { _ = peer.Close() })
	tm := newTestTunnelManager(t, nil)
	tm.log = log
	tm.addr = "127.0.0.1:1"
	tm.conn = serverConn
	tm.gen = 1

	cfg := config.Gateway{RealtimeAddr: "127.0.0.1:0", MaxConns: 8, AckClientFrames: true, TransportKeepaliveSec: 3600}
	srv = &RealtimeServer{
		cfg:    cfg,
		store:  store,
		log:    log,
		tm:     tm,
		sem:    make(chan struct{}, cfg.MaxConns),
		owners: map[string]uint64{},
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("realtime 监听失败: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { _ = srv.Close() })
	return srv.RealAddr(), srv, logs
}

// sessionCount 当前在线会话数。
func sessionCount(srv *RealtimeServer) int {
	n := 0
	srv.sids.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// waitSessionCount 等在线会话数达到 want。
func waitSessionCount(t *testing.T, srv *RealtimeServer, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sessionCount(srv) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("在线会话数没有达到预期：实际 %d 条，期望 %d 条", sessionCount(srv), want)
}

// waitLog 等日志里出现指定内容。
func waitLog(t *testing.T, logs *syncBuffer, want string) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), want) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// dialRaw 建连并完成握手（认证 → 问候 → hermes）。
func dialRaw(t *testing.T, addr, token string, reconnect bool) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("连接 realtime 失败: %v", err)
	}
	flag := byte(0x00)
	if reconnect {
		flag = 0x01
	}
	auth := append([]byte{proto.AuthMagic, flag}, proto.PackU32(uint32(len(token)))...)
	auth = append(auth, token...)
	if _, err := conn.Write(auth); err != nil {
		t.Fatalf("发送认证包失败: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	greeting := make([]byte, 36)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		t.Fatalf("读问候失败: %v", err)
	}
	if string(greeting) != proto.GreetingUUID {
		t.Fatalf("问候不符: %q", greeting)
	}
	if _, err := conn.Write(proto.BuildFrame(0, 1, []byte("<hermes></hermes>"))); err != nil {
		t.Fatalf("发送 hermes 帧失败: %v", err)
	}
	return conn
}

// TestPanicInHandlerIsolated 锁住「会话 goroutine 里 panic 无人兜底」。
//
// 事故形态：handler 或回调里的一次 panic 没有 recover，整个 gateway 进程
// 被带走，全部在线连接同时掉线。兜底之后应当是：出事的会话自己关掉并记日志，
// 其它会话照常运行，网关继续接受新连接。
func TestPanicInHandlerIsolated(t *testing.T) {
	const badToken = "boom|tok|999|1"
	inner := NewMemStore(1)
	// realtime 认证只认已登记的 token：三个探针 token 都要先签发，
	// 否则连接会在握手阶段被拒，测不到会话建立之后的路径。
	for _, tok := range []string{badToken, "good|tok|123|1", "ok|tok|456|1"} {
		if err := inner.SaveToken(context.Background(), tok, TokenInfo{Userid: 1000001}, time.Hour); err != nil {
			t.Fatalf("登记探针 token 失败: %v", err)
		}
	}
	store := &panicLookupStore{Store: inner, token: badToken}
	addr, srv, logs := startRealtimeOnly(t, store)

	good := dialRaw(t, addr, "good|tok|123|1", false)
	defer good.Close()
	waitSessionCount(t, srv, 1)

	// 出事连接：认证包触发的查询 panic。
	bad, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("连接 realtime 失败: %v", err)
	}
	defer bad.Close()
	auth := append([]byte{proto.AuthMagic, 0x00}, proto.PackU32(uint32(len(badToken)))...)
	auth = append(auth, badToken...)
	if _, err := bad.Write(auth); err != nil {
		t.Fatalf("发送认证包失败: %v", err)
	}

	if !waitLog(t, logs, "会话 goroutine 异常退出") {
		t.Fatalf("handler panic 没有被兜底记录，日志内容: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "模拟 handler 异常") {
		t.Fatalf("日志里缺少 panic 内容，无法定位事故: %s", logs.String())
	}

	// 出事的连接必须被关闭。
	_ = bad.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 8)
	if _, err := bad.Read(buf); err == nil {
		t.Fatal("出事的会话没有被关闭：连接仍然可读")
	}

	// 其它会话仍在，网关还能接新连接（进程没有被 panic 带走）。
	if got := sessionCount(srv); got != 1 {
		t.Fatalf("panic 波及了其它会话：在线 %d 条，期望 1 条", got)
	}
	third := dialRaw(t, addr, "ok|tok|456|1", false)
	defer third.Close()
	waitSessionCount(t, srv, 2)
}

// TestPanicInWriterIsolated 锁住「写者 goroutine 里 panic 无人兜底」。
//
// 承接待保存时写者会调存储；存储一旦 panic，没人 recover 就带走整个进程。
// 兜底之后：写者那一侧只关掉本会话并记日志，同进程的其它会话不受影响。
func TestPanicInWriterIsolated(t *testing.T) {
	const badToken = "boomw|tok|999|1"
	inner := NewMemStore(1)
	// 同上：good 与 bad 都要先签发。
	for _, tok := range []string{badToken, "good|tok|123|1"} {
		if err := inner.SaveToken(context.Background(), tok, TokenInfo{Userid: 1000001}, time.Hour); err != nil {
			t.Fatalf("登记探针 token 失败: %v", err)
		}
	}
	key := []byte(badToken)
	rc4In, err := proto.NewRC4(key)
	if err != nil {
		t.Fatalf("初始化 RC4 失败: %v", err)
	}
	rc4Out, _ := proto.NewRC4(key)
	if err := inner.SaveHandoff(context.Background(), badToken, Handoff{
		RC4In: rc4In.Export(), RC4Out: rc4Out.Export(), NextWireID: 5,
	}, time.Hour); err != nil {
		t.Fatalf("预置承接状态失败: %v", err)
	}
	store := &panicSaveStore{Store: inner, token: badToken}
	addr, srv, logs := startRealtimeOnly(t, store)

	good := dialRaw(t, addr, "good|tok|123|1", false)
	defer good.Close()
	waitSessionCount(t, srv, 1)

	// 重连接管会话：resumed=true，写者退出时必然保存快照 → 触发 panic。
	bad := dialRaw(t, addr, badToken, true)
	waitSessionCount(t, srv, 2)
	_ = bad.Close()

	if !waitLog(t, logs, "会话 goroutine 异常退出") {
		t.Fatalf("写者 panic 没有被兜底记录，日志内容: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "模拟写者异常") {
		t.Fatalf("日志里缺少写者 panic 内容，无法定位事故: %s", logs.String())
	}

	waitSessionCount(t, srv, 1)
	if got := sessionCount(srv); got != 1 {
		t.Fatalf("写者 panic 波及了其它会话：在线 %d 条，期望 1 条", got)
	}
	// 其它会话仍然可用：还能往这条连接上写帧。
	if _, err := good.Write(proto.BuildFrame(1, 1, nil)); err != nil {
		t.Fatalf("其它会话被写者 panic 一起断开了: %v", err)
	}
}
