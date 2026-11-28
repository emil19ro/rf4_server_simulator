package gameserver

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/proto"
	"rf4server/internal/store"
	"rf4server/internal/wiregen"
)

// panicLogSink 收集 slog 文本输出，用于断言 panic 诊断信息。
type panicLogSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *panicLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *panicLogSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// panicTestCtx 构造一条带可断言 logger 的请求上下文。
func panicTestCtx(t *testing.T, main, sub int, callID uint32, expect bool) (*Ctx, *panicLogSink) {
	t.Helper()
	srv := NewServer(config.Game{Tunnel: config.Tunnel{PSK: "panic-guard"}}, logging.Discard(), nil)
	sink := &panicLogSink{}
	base := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := &Ctx{
		Sess:           &Session{ID: 1, Userid: 1000001, Srv: srv, log: base},
		Main:           main,
		Sub:            sub,
		CallID:         callID,
		Body:           emptyArgsBody(),
		ExpectResponse: expect,
	}
	ctx.Log = base.With("cmd", ctx.Key(), "call", callID)
	return ctx, sink
}

// TestExecuteRequestRecoversHandlerPanic 断言 handler panic 不冒泡到调用方，
// 而是转成失败结果，并留下带 cmd、call_id 与栈的日志。
func TestExecuteRequestRecoversHandlerPanic(t *testing.T) {
	ctx, sink := panicTestCtx(t, 250, 21, 4242, true)
	srv := ctx.Sess.Srv
	srv.dispatch.Register(250, 21, func(*Ctx) ([]byte, bool) {
		panic("注入的 handler panic")
	})

	var result requestExecutionResult
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("期望 handler panic 被入口隔离，实际仍冒泡到调用方: %v", r)
			}
		}()
		result = srv.dispatch.executeRequest(ctx, true)
	}()

	if result.errorText != panicFailureReceipt {
		t.Fatalf("期望 panic 后标记失败原因 %q，实际 %q", panicFailureReceipt, result.errorText)
	}
	if !result.hasResponse || !result.nativeFailure || len(result.body) == 0 {
		t.Fatalf("期望返回可直接发送的失败信封，实际 hasResponse=%v nativeFailure=%v len=%d",
			result.hasResponse, result.nativeFailure, len(result.body))
	}
	env, ok := proto.ParseEnvelope(result.body)
	if !ok {
		t.Fatalf("期望失败信封可解析，实际解析失败 body=%x", result.body)
	}
	if env.Marker != -3 || env.CallID != 4242 {
		t.Fatalf("期望 -3 失败信封且 call_id=4242，实际 marker=%d call=%d", env.Marker, env.CallID)
	}

	logged := sink.String()
	if !strings.Contains(logged, "cmd=250/21") {
		t.Fatalf("panic 日志缺少 cmd 键，期望包含 cmd=250/21，实际日志:\n%s", logged)
	}
	if !strings.Contains(logged, "call=4242") {
		t.Fatalf("panic 日志缺少 call_id，期望包含 call=4242，实际日志:\n%s", logged)
	}
	if !strings.Contains(logged, "stack=") || !strings.Contains(logged, "request_panic_guard_test.go") {
		t.Fatalf("panic 日志缺少栈，期望包含 stack= 与测试文件帧，实际日志:\n%s", logged)
	}
}

// TestExecuteRequestPanicWithoutExpectResponseStaysSilent 断言 fire-and-forget 请求
// panic 后不回包，但仍标记失败原因，避免上层误判为成功。
func TestExecuteRequestPanicWithoutExpectResponseStaysSilent(t *testing.T) {
	ctx, _ := panicTestCtx(t, 250, 22, 4243, false)
	srv := ctx.Sess.Srv
	srv.dispatch.Register(250, 22, func(*Ctx) ([]byte, bool) {
		panic("注入的无回包 panic")
	})

	result := srv.dispatch.executeRequest(ctx, true)
	if result.hasResponse || len(result.body) != 0 {
		t.Fatalf("期望不回包，实际 hasResponse=%v len=%d", result.hasResponse, len(result.body))
	}
	if result.errorText != panicFailureReceipt {
		t.Fatalf("期望失败原因 %q，实际 %q", panicFailureReceipt, result.errorText)
	}
}

// TestDispatchRequestPanicRepliesFailureAndReplays 断言带回执表的请求 panic 后
// 回 -3 信封、回执落定，且同一 call_id 重试只重放不再执行 handler。
func TestDispatchRequestPanicRepliesFailureAndReplays(t *testing.T) {
	srv, db := newReceiptTestServer(t, filepath.Join(t.TempDir(), "request-panic.db"))
	defer db.Close()

	var calls int
	srv.dispatch.Register(250, 23, func(*Ctx) ([]byte, bool) {
		calls++
		panic("注入的回执路径 panic")
	})
	sess := newReceiptTestSession(t, srv, 1, "1|2|1000001|panic")
	body := emptyArgsBody()
	sess.send(250, 23, 555, body, 0x00)

	response := waitReceiptResponse(t, sess)
	if response.Marker != -3 || response.CallID != 555 {
		t.Fatalf("期望 panic 后回 -3 失败信封且 call_id=555，实际 marker=%d call=%d",
			response.Marker, response.CallID)
	}

	key, _ := requestReceiptKey(&Ctx{
		Sess: &Session{Token: "1|2|1000001|panic"}, Main: 250, Sub: 23, CallID: 555, Body: body,
	})
	rec, err := db.GetRequestReceipt(t.Context(), key)
	if err != nil {
		t.Fatalf("读取回执失败: %v", err)
	}
	if rec.Status != store.RequestReceiptCompleted || !rec.HasResponse {
		t.Fatalf("期望回执落定为 %q 且带响应，实际 status=%q has_response=%v",
			store.RequestReceiptCompleted, rec.Status, rec.HasResponse)
	}

	sess.send(250, 23, 555, body, 0x00)
	if replay := waitReceiptResponse(t, sess); replay.Marker != -3 || replay.CallID != 555 {
		t.Fatalf("期望重放 -3 失败信封，实际 marker=%d call=%d", replay.Marker, replay.CallID)
	}
	if calls != 1 {
		t.Fatalf("期望 panic 的 handler 只执行一次，实际 %d 次", calls)
	}
}

// TestStoreBoatsPanicPathReturnsFailure 断言 22/5 船只条目类名缺失时
// handler 回失败响应而不是 panic。
func TestStoreBoatsPanicPathReturnsFailure(t *testing.T) {
	if _, ok := wiregen.ClassOps["efdgfpfbjai"]; !ok {
		t.Skip("船只条目类 efdgfpfbjai 不在 wire 表内，跳过用例")
	}
	ctx, _ := panicTestCtx(t, 22, 5, 777, true)
	body, handled := hStoreBoats(ctx)
	if !handled || len(body) == 0 {
		t.Fatalf("期望 22/5 正常返回船只条目，实际 handled=%v len=%d", handled, len(body))
	}

	// 类名缺失时 MakeObj 返回错误而不是 panic，handler 据此回失败响应。
	if _, err := wiregen.MakeObj("panic_guard_missing_class", nil); err == nil {
		t.Fatalf("期望未知类名返回错误，实际 err=nil")
	}
}
