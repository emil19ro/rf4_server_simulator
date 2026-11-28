package gateway

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"rf4server/internal/config"
)

// TestNewRealtimeServerFallsBackOnInvalidMaxConns 保证非法 max_conns 不会打死接入面。
//
// 事故形态：max_conns: 0 时 sem 是无缓冲 channel，Accept 后的 sem <- struct{}{}
// 从第二条连接起永久阻塞，所有客户端握手卡死；负数则让 make(chan, -1) 直接 panic，
// gateway 启动即挂。构造函数必须落到正容量，并且不能静默——要留下可见告警。
func TestNewRealtimeServerFallsBackOnInvalidMaxConns(t *testing.T) {
	for _, n := range []int{0, -5} {
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, nil))

		srv := NewRealtimeServer(config.Gateway{MaxConns: n}, NewMemStore(1000001), log, nil, nil)

		if cap(srv.sem) <= 0 {
			t.Fatalf("max_conns=%d 时 sem 容量=%d，接入循环会卡死", n, cap(srv.sem))
		}
		if cap(srv.sem) != config.DefaultMaxConns {
			t.Fatalf("max_conns=%d 时未落到默认值，sem 容量=%d", n, cap(srv.sem))
		}
		if srv.cfg.MaxConns != config.DefaultMaxConns {
			t.Fatalf("max_conns=%d 时实例配置未落到默认值：%d", n, srv.cfg.MaxConns)
		}

		out := buf.String()
		if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "gateway.max_conns") {
			t.Fatalf("max_conns=%d 时没有可见告警，实际日志：%q", n, out)
		}
	}
}

// TestNewRealtimeServerKeepsValidMaxConns 保证合法值原样生效且不产生告警。
func TestNewRealtimeServerKeepsValidMaxConns(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	srv := NewRealtimeServer(config.Gateway{MaxConns: 7}, NewMemStore(1000001), log, nil, nil)

	if cap(srv.sem) != 7 || srv.cfg.MaxConns != 7 {
		t.Fatalf("合法值被改写：cfg=%d sem=%d", srv.cfg.MaxConns, cap(srv.sem))
	}
	if buf.Len() != 0 {
		t.Fatalf("合法值不应告警：%q", buf.String())
	}
}
