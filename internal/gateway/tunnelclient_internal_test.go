package gateway

import (
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"rf4server/internal/metrics"
	"rf4server/internal/tunnel"
)

// newTestTunnelManager 造一个只跑读循环的 TunnelManager。
//
// 直接构造字面量而不是 NewTunnelManager，是因为这里不需要拨号能力；指标句柄
// 随实例持有，必须显式初始化——读循环收尾会写 tl.up，为零值会空指针。
func newTestTunnelManager(t *testing.T, onState func(up bool)) *TunnelManager {
	t.Helper()
	return &TunnelManager{
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		onState: onState,
		tl:      tunnelMetricsFor(prometheus.NewRegistry()),
	}
}

// dialTunnelPair 造一对已握手的隧道连接：返回服务端侧 *tunnel.Conn 与其对端。
//
// 用真实 Dial/Handshake 而不是直接构造 Conn，是因为 Conn 的内部字段不导出，
// 也不希望为了测试把 newConn 提升为公开 API。
func dialTunnelPair(t *testing.T, psk string) (server *tunnel.Conn, peer *tunnel.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	type res struct {
		c   *tunnel.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			ch <- res{err: err}
			return
		}
		c, err := tunnel.HandshakeServer(nc, psk)
		ch <- res{c: c, err: err}
	}()

	cli, err := tunnel.Dial(ln.Addr().String(), psk)
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	// 掐断客户端侧，服务端 ReadRecord 就会返回错误，读循环随即退出。
	return r.c, cli
}

// TestOldReadLoopDoesNotClobberNewConn 锁住 P2-2：旧连接的读循环退出时，
// 若当前连接已经换成新的，它不得改动任何全局状态。
//
// 事故形态：getConn 发现旧连接失效后会先建好新连接，旧连接的读循环这时才
// 退出。它的 defer 无条件清隧道在线指标、报离线、并 dropAll —— 于是刚在新
// 隧道上建起来的健康会话被全部杀掉，客户端看到的就是又一次「突然掉线」。
func TestOldReadLoopDoesNotClobberNewConn(t *testing.T) {
	const psk = "internal-test-psk"
	oldConn, oldPeer := dialTunnelPair(t, psk)
	newConn, _ := dialTunnelPair(t, psk)

	var mu sync.Mutex
	var states []bool
	m := newTestTunnelManager(t, func(up bool) { mu.Lock(); states = append(states, up); mu.Unlock() })

	// 新连接已是当前连接（代际 2）；旧连接是代际 1，它的读循环稍后才退出。
	m.conn = newConn
	m.gen = 2

	// 挂一个「健康会话」在新隧道上，dropAll 会把它关掉。
	rt := &RealtimeServer{log: m.log}
	left, right := net.Pipe()
	defer right.Close()
	sess := &clientSession{id: 7, conn: left, srv: rt, log: m.log}
	rt.sids.Store(sess.id, sess)
	m.gw = rt

	done := make(chan struct{})
	go func() { m.readLoop(oldConn, 1); close(done) }()

	// 掐断旧连接的对端，读循环读到错误后走 defer。
	_ = oldPeer.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("旧连接的读循环没有退出")
	}

	if m.conn != newConn {
		t.Fatal("旧连接的读循环把当前连接清掉了：新隧道会被当成断开，随后的会话全部重连")
	}
	if sess.closed.Load() {
		t.Fatal("旧连接的读循环 dropAll 杀掉了新隧道上的健康会话：玩家会看到又一次突然掉线")
	}
	mu.Lock()
	n := len(states)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("旧连接的读循环上报了离线状态 %d 次：隧道在线指标会被打错", n)
	}
}

// TestCurrentReadLoopStillReportsOffline 确认收紧判断后没有把正常路径改坏：
// 当前连接断开时仍要清状态、报离线、踢掉会话。
func TestCurrentReadLoopStillReportsOffline(t *testing.T) {
	const psk = "internal-test-psk"
	cur, peer := dialTunnelPair(t, psk)

	var mu sync.Mutex
	var states []bool
	m := newTestTunnelManager(t, func(up bool) { mu.Lock(); states = append(states, up); mu.Unlock() })
	m.conn = cur
	m.gen = 1

	rt := &RealtimeServer{log: m.log}
	left, right := net.Pipe()
	defer right.Close()
	sess := &clientSession{id: 9, conn: left, srv: rt, log: m.log}
	rt.sids.Store(sess.id, sess)
	m.gw = rt

	done := make(chan struct{})
	go func() { m.readLoop(cur, 1); close(done) }()

	_ = peer.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("当前连接的读循环没有退出")
	}

	if m.conn != nil {
		t.Fatal("当前连接断开后应清空 m.conn，否则后续 send 会一直打到死连接上")
	}
	if !sess.closed.Load() {
		t.Fatal("当前隧道断开必须踢掉会话，否则客户端会挂在半死状态上")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(states) != 1 || states[0] != false {
		t.Fatalf("当前连接断开应上报一次离线，实际 %v", states)
	}
}

// TestWriteFailureStillDropsSessions 锁住「send 清空 m.conn 后读循环仍须清理」。
//
// 事故形态：send() 写失败时先把 m.conn 清成 nil 再关连接，读循环随后退出看到
// m.conn == nil。若收尾判据写成「m.conn != c 就当已被接替」，nil 会被误判成
// 「已有新连接」，于是离线回调与 dropAll 全部跳过：主服那边早已没有对应隧道
// 会话，网关侧会话却还挂着，玩家的后续请求全部静默丢失，且永不重连。
func TestWriteFailureStillDropsSessions(t *testing.T) {
	const psk = "internal-test-psk"
	cur, peer := dialTunnelPair(t, psk)

	var mu sync.Mutex
	var states []bool
	m := newTestTunnelManager(t, func(up bool) { mu.Lock(); states = append(states, up); mu.Unlock() })
	m.conn = cur
	m.gen = 1

	rt := &RealtimeServer{log: m.log}
	left, right := net.Pipe()
	defer right.Close()
	sess := &clientSession{id: 11, conn: left, srv: rt, log: m.log}
	rt.sids.Store(sess.id, sess)
	m.gw = rt

	done := make(chan struct{})
	go func() { m.readLoop(cur, 1); close(done) }()

	// 复刻 send() 的写失败路径：先清 m.conn，再关连接。
	m.mu.Lock()
	m.conn = nil
	m.mu.Unlock()
	_ = peer.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("读循环没有退出")
	}

	if !sess.closed.Load() {
		t.Fatal("写失败清空连接后读循环跳过了 dropAll：会话会一直挂在一个主服已不存在的隧道会话上")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(states) != 1 || states[0] != false {
		t.Fatalf("写失败后仍应上报一次离线，实际 %v", states)
	}
}

// startTunnelDialServer 起一个只做握手的隧道服务端，返回监听地址。
//
// 用于让多个 TunnelManager 各自真实拨一次号，从而驱动拨号与发送路径上的指标。
func startTunnelDialServer(t *testing.T, psk string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				c, err := tunnel.HandshakeServer(nc, psk)
				if err != nil {
					_ = nc.Close()
					return
				}
				defer func() { _ = c.Close() }()
				<-c.Done()
			}()
		}
	}()
	return ln.Addr().String()
}

// gatherTunnel 采集注册表里的隧道指标，键形如 name/label=value。
func gatherTunnel(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("采集隧道指标失败: %v", err)
	}
	out := make(map[string]float64, len(mfs))
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			key := mf.GetName()
			for _, lp := range m.GetLabel() {
				key += "/" + lp.GetName() + "=" + lp.GetValue()
			}
			if g := m.GetGauge(); g != nil {
				out[key] = g.GetValue()
			}
			if c := m.GetCounter(); c != nil {
				out[key] = c.GetValue()
			}
		}
	}
	return out
}

// TestTunnelMetricsArePerInstance 锁住「指标句柄归实例所有」：同进程内构造两个
// TunnelManager 各自拨号后，各自的指标必须记在各自的注册表上，互不覆盖。
func TestTunnelMetricsArePerInstance(t *testing.T) {
	const psk = "internal-test-psk"
	addr := startTunnelDialServer(t, psk)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 两个实例各用一份观测端口：指标应各记各的，互不覆盖。
	obs1 := metrics.NewServer("127.0.0.1:0", "gateway-test-1")
	obs2 := metrics.NewServer("127.0.0.1:0", "gateway-test-2")
	m1 := NewTunnelManager(addr, psk, log, obs1, nil)
	m2 := NewTunnelManager(addr, psk, log, obs2, nil)
	t.Cleanup(func() {
		m1.Close()
		m2.Close()
	})

	if err := m1.send(tunnel.TypeData, 1, []byte("a")); err != nil {
		t.Fatalf("实例一发送失败: %v", err)
	}
	if err := m1.send(tunnel.TypePing, 1, nil); err != nil {
		t.Fatalf("实例一发送 PING 失败: %v", err)
	}
	if err := m2.send(tunnel.TypeClose, 2, []byte("b")); err != nil {
		t.Fatalf("实例二发送失败: %v", err)
	}

	got1 := gatherTunnel(t, obs1.Registry())
	got2 := gatherTunnel(t, obs2.Registry())
	if v, ok := got1["gateway_tunnel_up"]; !ok || v != 1 {
		t.Fatalf("实例一拨号成功后在线指标应为 1，实际 %v（存在=%v）", v, ok)
	}
	if v, ok := got2["gateway_tunnel_up"]; !ok || v != 1 {
		t.Fatalf("实例二拨号成功后在线指标应为 1，实际 %v（存在=%v）", v, ok)
	}
	if v := got1["gateway_tunnel_dials_total"]; v != 1 {
		t.Fatalf("实例一应记 1 次拨号，实际 %v", v)
	}
	if v := got2["gateway_tunnel_dials_total"]; v != 1 {
		t.Fatalf("实例二应记 1 次拨号，实际 %v", v)
	}
	if v := got1["gateway_tunnel_sent_total/type=DATA"]; v != 1 {
		t.Fatalf("实例一应记 1 条 DATA 发送，实际 %v", v)
	}
	if v := got1["gateway_tunnel_sent_total/type=PING"]; v != 1 {
		t.Fatalf("实例一应记 1 条 PING 发送，实际 %v", v)
	}
	if v := got2["gateway_tunnel_sent_total/type=CLOSE"]; v != 1 {
		t.Fatalf("实例二应记 1 条 CLOSE 发送，实际 %v", v)
	}
	if _, ok := got2["gateway_tunnel_sent_total/type=DATA"]; ok {
		t.Fatal("实例一的发送记到了实例二的指标上：多实例指标串台")
	}
	if _, ok := got1["gateway_tunnel_sent_total/type=CLOSE"]; ok {
		t.Fatal("实例二的发送记到了实例一的指标上：多实例指标串台")
	}
}

// TestNewTunnelManagerRepeatedInProcess 锁住「同进程多次构造不 panic」：
// obs 为 nil 时观测端口未启用，指标仍需注册到一份稳定、可读写的注册表上，
// 重复构造不得因同名指标二次注册而 panic，也不得把指标丢进一次性注册表。
func TestNewTunnelManagerRepeatedInProcess(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	first := NewTunnelManager("127.0.0.1:1", "internal-test-psk", log, nil, nil)
	second := NewTunnelManager("127.0.0.1:1", "internal-test-psk", log, nil, nil)
	t.Cleanup(func() {
		first.Close()
		second.Close()
	})
	if first.tl.up != second.tl.up {
		t.Fatal("同一进程内的多个实例应共用一份隧道指标，而不是各注册一套")
	}
	first.tl.up.Set(1)
	defer first.tl.up.Set(0)
	if got := gatherTunnel(t, tunnelRegistryOf(nil))["gateway_tunnel_up"]; got != 1 {
		t.Fatalf("未启用观测端口时指标仍应可读写，期望在线指标 1，实际 %v", got)
	}
}

// TestTunnelManagerConstructRace 用 -race 锁住「构造 TunnelManager」与「读循环
// 收尾」并发的情形：构造不得与另一个实例的读循环共享可写状态。
//
// 读循环是构造方随连接起的后台 goroutine，谁也不会等它退出；因此这里同样只
// 启动、不同步等待，让「收尾写指标」与「构造时初始化指标」真正处在并发窗口里。
// 一旦两步操作同一份包级状态，-race 就会把它报出来。
func TestTunnelManagerConstructRace(t *testing.T) {
	const psk = "internal-test-psk"
	cur, peer := dialTunnelPair(t, psk)

	m := newTestTunnelManager(t, nil)
	m.conn = cur
	m.gen = 1

	done := make(chan struct{})
	go func() {
		m.readLoop(cur, 1)
		close(done)
	}()

	// 掐断对端，读循环随即走到收尾；不等待它，构造与收尾必须靠各自的同步保证安全。
	_ = peer.Close()
	time.Sleep(20 * time.Millisecond)
	for i := 0; i < 4; i++ {
		NewTunnelManager("127.0.0.1:0", psk, m.log, nil, nil)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("读循环没有退出")
	}
}
