package metrics

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

// readyzCode 通过内部 handler 打一次 /readyz，返回状态码。
func readyzCode(s *Server) int {
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec.Code
}

// TestServerReadyFieldIsAtomic 是普通 go test 也能抓到的确定性回归。
//
// SetReady 由主 goroutine 调用，/readyz handler 在观测端口的 goroutine 读取；
// 字段退回普通 bool 后只有 -race 能报 data race，而 CI 门禁不跑 -race。
func TestServerReadyFieldIsAtomic(t *testing.T) {
	field, ok := reflect.TypeOf(Server{}).FieldByName("ready")
	if !ok {
		t.Fatal("Server 缺少 ready 字段")
	}
	want := reflect.TypeOf(atomic.Bool{})
	if field.Type != want {
		t.Fatalf("Server.ready 类型 = %v，期望 %v；普通 bool 会在 /readyz 与 SetReady 之间形成 data race", field.Type, want)
	}
}

// TestReadyzReflectsSetReady 未就绪时必须 503，SetReady(true) 后必须 200。
//
// 三个进程都是先起观测端口、再在 main 里调 SetReady(true)：如果探针恒返回
// 200，「依赖没就绪」这一状态在外部就完全不可见。
func TestReadyzReflectsSetReady(t *testing.T) {
	s := NewServer(":0", "gateway")

	if code := readyzCode(s); code != http.StatusServiceUnavailable {
		t.Fatalf("NewServer 后未 SetReady，/readyz 状态码 = %d，期望 %d", code, http.StatusServiceUnavailable)
	}
	s.SetReady(true)
	if code := readyzCode(s); code != http.StatusOK {
		t.Fatalf("SetReady(true) 后 /readyz 状态码 = %d，期望 %d", code, http.StatusOK)
	}
	s.SetReady(false)
	if code := readyzCode(s); code != http.StatusServiceUnavailable {
		t.Fatalf("SetReady(false) 后 /readyz 状态码 = %d，期望 %d", code, http.StatusServiceUnavailable)
	}
}

// TestReadyzConcurrentSetReadyNoRace 并发读写就绪标志，由 -race 判定有没有数据竞争。
//
// 真实形态是 main goroutine 调 SetReady，同时观测端口的 handler goroutine
// 在处理 /readyz；普通 bool 字段在这里就是一处 race。
func TestReadyzConcurrentSetReadyNoRace(t *testing.T) {
	s := NewServer(":0", "gameserver")

	const rounds = 200
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			s.SetReady(true)
			s.SetReady(false)
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if code := readyzCode(s); code != http.StatusOK && code != http.StatusServiceUnavailable {
				t.Errorf("/readyz 状态码 = %d，期望 200 或 503", code)
				return
			}
		}
	}()

	wg.Wait()
}
