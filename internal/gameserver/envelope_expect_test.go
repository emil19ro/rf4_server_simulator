package gameserver

import (
	"bytes"
	"testing"
	"time"

	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/proto"
)

// newExpectTailTestServer 起一个不接 DB 的服务器：入站请求走 DB==nil 分支，
// 仍然完整执行 dispatchRequest，可以直接观察 ExpectResponse 对是否回包的影响。
func newExpectTailTestServer(t *testing.T) *Server {
	t.Helper()
	return NewServer(config.Game{Tunnel: config.Tunnel{PSK: "expect-tail"}}, logging.Discard(), nil)
}

func waitExpectTailProbe(t *testing.T, seen <-chan *Ctx) *Ctx {
	t.Helper()
	select {
	case ctx := <-seen:
		return ctx
	case <-time.After(2 * time.Second):
		t.Fatal("handler 未被调用")
		return nil
	}
}

func waitExpectTailEnvelope(t *testing.T, h *mailTestSession) *proto.Envelope {
	t.Helper()
	select {
	case env, ok := <-h.envelopes:
		if !ok {
			t.Fatal("测试隧道已关闭")
		}
		return env
	case <-time.After(2 * time.Second):
		t.Fatal("未收到响应")
		return nil
	}
}

func assertNoExpectTailEnvelope(t *testing.T, h *mailTestSession) {
	t.Helper()
	select {
	case env := <-h.envelopes:
		t.Fatalf("不应收到回包: marker=%d call=%d body=%x", env.Marker, env.CallID, env.Body)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestEnvelopeExpectTailControlsReply expect_response 尾字节直接决定是否回包：
// 0x00 期望响应必须回包，0x01（fire-and-forget）只执行 handler、绝不回包。
func TestEnvelopeExpectTailControlsReply(t *testing.T) {
	const uid int64 = 1000001
	s := newExpectTailTestServer(t)
	seen := make(chan *Ctx, 4)
	want := emptyArgsBody()
	s.dispatch.Register(250, 1, func(ctx *Ctx) ([]byte, bool) {
		seen <- ctx
		return want, true
	})
	h := newMailTestSession(t, s, 1, uid)
	request := func(callID uint32, body []byte) {
		t.Helper()
		s.handleEnvelopePayload(h.sess, proto.BuildRequestEnvelope(callID, 250, 1, body))
	}

	t.Run("尾字节0x00期望响应", func(t *testing.T) {
		request(11, append(append([]byte{}, want...), 0x00))
		ctx := waitExpectTailProbe(t, seen)
		if !ctx.ExpectResponse {
			t.Fatal("0x00 应判为期望响应")
		}
		if !bytes.Equal(ctx.Body, want) {
			t.Fatalf("handler 收到的 body 含尾字节: %x", ctx.Body)
		}
		env := waitExpectTailEnvelope(t, h)
		if env.Marker != -2 || env.CallID != 11 || !bytes.Equal(env.Body, want) {
			t.Fatalf("响应不符: marker=%d call=%d body=%x", env.Marker, env.CallID, env.Body)
		}
	})

	t.Run("尾字节0x01不回包", func(t *testing.T) {
		request(12, append(append([]byte{}, want...), 0x01))
		ctx := waitExpectTailProbe(t, seen)
		if ctx.ExpectResponse {
			t.Fatal("0x01 应判为 fire-and-forget")
		}
		if !bytes.Equal(ctx.Body, want) {
			t.Fatalf("handler 收到的 body 含尾字节: %x", ctx.Body)
		}
		assertNoExpectTailEnvelope(t, h)
	})
}

// TestEnvelopeUnknownTailKeepsBody 尾字节不在 {0,1} 时不能当 expect_response：
// 不许丢字节，也不许退化成 fire-and-forget（否则客户端一直等不到回包）。
func TestEnvelopeUnknownTailKeepsBody(t *testing.T) {
	const uid int64 = 1000001
	s := newExpectTailTestServer(t)
	seen := make(chan *Ctx, 4)
	want := emptyArgsBody()
	s.dispatch.Register(250, 1, func(ctx *Ctx) ([]byte, bool) {
		seen <- ctx
		return want, true
	})
	h := newMailTestSession(t, s, 1, uid)

	body := append(append([]byte{}, want...), 0x7f)
	s.handleEnvelopePayload(h.sess, proto.BuildRequestEnvelope(13, 250, 1, body))
	ctx := waitExpectTailProbe(t, seen)
	if !bytes.Equal(ctx.Body, body) {
		t.Fatalf("未识别尾字节时丢了 body 末尾字节: got=%x want=%x", ctx.Body, body)
	}
	if !ctx.ExpectResponse {
		t.Fatal("未识别尾字节应按期望响应处理，不能判成 fire-and-forget")
	}
	env := waitExpectTailEnvelope(t, h)
	if env.Marker != -2 || env.CallID != 13 || !bytes.Equal(env.Body, want) {
		t.Fatalf("响应不符: marker=%d call=%d body=%x", env.Marker, env.CallID, env.Body)
	}
}

// TestEnvelopeMissingTailExpectsResponse 完全不带尾字节时按期望响应处理。
func TestEnvelopeMissingTailExpectsResponse(t *testing.T) {
	const uid int64 = 1000001
	s := newExpectTailTestServer(t)
	seen := make(chan *Ctx, 4)
	want := emptyArgsBody()
	s.dispatch.Register(250, 1, func(ctx *Ctx) ([]byte, bool) {
		seen <- ctx
		return want, true
	})
	h := newMailTestSession(t, s, 1, uid)

	s.handleEnvelopePayload(h.sess, proto.BuildRequestEnvelope(14, 250, 1, nil))
	ctx := waitExpectTailProbe(t, seen)
	if !ctx.ExpectResponse || len(ctx.Body) != 0 {
		t.Fatalf("缺尾字节的信封应判为期望响应且 body 为空: expect=%v body=%x", ctx.ExpectResponse, ctx.Body)
	}
	env := waitExpectTailEnvelope(t, h)
	if env.Marker != -2 || env.CallID != 14 {
		t.Fatalf("响应不符: marker=%d call=%d", env.Marker, env.CallID)
	}
}
