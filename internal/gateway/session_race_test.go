package gateway

import (
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"rf4server/internal/config"
	"rf4server/internal/proto"
)

// testMetricsOnce 保证包级指标只初始化一次。
// 指标是包级变量，反复初始化会与仍在运行的 goroutine 构成无同步读写。
var testMetricsOnce sync.Once

// ensureTestMetrics 初始化 realtime 指标（幂等）。
func ensureTestMetrics() {
	testMetricsOnce.Do(func() { initRealtimeMetrics(obsRegistry(nil)) })
}

var tunnelMetricsOnce sync.Once

// ensureTunnelMetrics 初始化隧道指标（幂等）。
func ensureTunnelMetrics() {
	tunnelMetricsOnce.Do(func() { _ = tunnelMetricsFor(obsRegistry(nil)) })
}

// newRaceTestSession 造一条只跑写者、不接真实网络的会话。
func newRaceTestSession(id uint64, conn net.Conn, srv *RealtimeServer) *clientSession {
	key := []byte("race-test-token")
	in, _ := proto.NewRC4(key)
	out, _ := proto.NewRC4(key)
	return &clientSession{
		id:         id,
		conn:       conn,
		token:      "race|tok|1|x",
		rc4In:      in,
		rc4Out:     out,
		outCh:      make(chan outbound, 1024),
		done:       make(chan struct{}),
		writerDone: make(chan struct{}),
		srv:        srv,
		log:        srv.log,
	}
}

// TestSessionTeardownEnqueueRaceNoPanic 锁住「会话收尾与隧道入队并发」的 TOCTOU。
//
// 事故形态：隧道读 goroutine 按 sid 取到会话后先判断 closed、再往 outCh 入队，
// 而会话收尾可能在两步之间关闭 outCh —— 入队方 panic: send on closed channel。
// 这个 panic 发生在业务 goroutine 里，没人 recover 就会带走整个 gateway 进程，
// 全部在线连接同时掉线。
//
// 这里跑 100 轮「写者运行中的收尾」与「数据/ACK 并发入队」：任何一轮出现
// send on closed channel 都会直接终止测试二进制，无需额外断言。
func TestSessionTeardownEnqueueRaceNoPanic(t *testing.T) {
	ensureTestMetrics()
	srv := &RealtimeServer{
		cfg:    config.Gateway{TransportKeepaliveSec: 3600},
		store:  NewMemStore(1),
		log:    slog.New(slog.DiscardHandler),
		owners: map[string]uint64{},
	}

	for round := 0; round < 100; round++ {
		server, client := net.Pipe()
		// 排空写者输出：net.Pipe 无缓冲，没人读会把写者卡在 Write 上。
		go func() { _, _ = io.Copy(io.Discard, client) }()

		sess := newRaceTestSession(uint64(round+1), server, srv)
		srv.claimOwnership(sess.token, sess.id)
		sess.writerUp.Store(true)
		go sess.writer()

		var wg sync.WaitGroup
		// 多个入队方与收尾并发：判定未收尾之后、真正入队之前，会话可能刚好
		// 收尾完毕，这正是原实现 panic 的窗口。
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				for j := 0; j < 50; j++ {
					sess.EnqueueData([]byte{byte(i), byte(j)})
				}
			}(i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				sess.enqueue(outbound{kind: outAck, wireID: uint64(j + 1)})
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess.drain()
		}()
		wg.Wait()

		_ = client.Close()
		_ = server.Close()
		srv.releaseOwnership(sess.token, sess.id)
	}
}

// TestAckEnqueueDoesNotBlockReadLoop 锁住「ACK 用阻塞发送」。
//
// 事故形态：ACK 直接写 outCh 且没有 default。会话收尾后写者已经退出，
// 队列不会再被消费；读循环就永久卡在这条发送上，客户端后续上行再没人处理，
// 直到连接超时才发现。
func TestAckEnqueueDoesNotBlockReadLoop(t *testing.T) {
	ensureTestMetrics()
	log := slog.New(slog.DiscardHandler)
	tm := newTestTunnelManager(t, nil)
	tm.log = log
	srv := &RealtimeServer{
		cfg:    config.Gateway{AckClientFrames: true, TransportKeepaliveSec: 3600},
		log:    log,
		tm:     tm,
		owners: map[string]uint64{},
	}
	server, client := net.Pipe()
	defer client.Close()

	sess := newRaceTestSession(1, server, srv)
	// 队列灌满且会话已进入收尾：写者不会再消费队列，任何入队都必然没有空位。
	for i := 0; i < cap(sess.outCh); i++ {
		sess.outCh <- outbound{kind: outData}
	}
	sess.closeOnce.Do(func() { close(sess.done) })

	frame := &proto.Frame{BodyLen: 9, FrameType: 0, WireID: 7, Payload: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9}}
	handled := make(chan struct{})
	go func() {
		sess.handleFrame(frame)
		close(handled)
	}()
	select {
	case <-handled:
	case <-time.After(2 * time.Second):
		t.Fatal("ACK 入队阻塞了读循环：会话收尾后写者已退出，客户端后续上行会永久挂死")
	}
}
