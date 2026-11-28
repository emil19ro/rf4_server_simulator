package supervisor

import (
	"errors"
	"sync"
	"testing"
)

// RestartOne 的 name 来自 HTTP query，没有白名单。未知进程名必须返回明确错误
// 而不是 panic：旧实现在 s.mu 之外直接解引用 s.procs[name]。
func TestRestartOneUnknownProcessReturnsError(t *testing.T) {
	s := New(t.TempDir(), "config.yaml", "")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RestartOne 未知进程名 panic: %v", r)
		}
	}()
	err := s.RestartOne("不存在的进程")
	if !errors.Is(err, ErrUnknownProcess) {
		t.Fatalf("err=%v, want ErrUnknownProcess", err)
	}
}

// StopOne/StartOne 对未知进程名的行为必须与 RestartOne 一致：返回 error，不 panic。
func TestStopOneAndStartOneUnknownProcessReturnError(t *testing.T) {
	s := New(t.TempDir(), "config.yaml", "")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("未知进程名 panic: %v", r)
		}
	}()
	if err := s.StopOne("不存在的进程"); !errors.Is(err, ErrUnknownProcess) {
		t.Fatalf("StopOne err=%v, want ErrUnknownProcess", err)
	}
	if err := s.StartOne("不存在的进程"); !errors.Is(err, ErrUnknownProcess) {
		t.Fatalf("StartOne err=%v, want ErrUnknownProcess", err)
	}
}

// procState 是状态读取的唯一入口：未知进程名返回空串，不 panic。
func TestProcStateUnknownProcessIsEmpty(t *testing.T) {
	s := New(t.TempDir(), "config.yaml", "")
	if got := s.procState("不存在的进程"); got != "" {
		t.Fatalf("procState=%q, want 空串", got)
	}
	if got := s.procState("gameserver"); got != StateStopped {
		t.Fatalf("procState=%q, want %q", got, StateStopped)
	}
}

// 一边 Status()/StopOne/RestartOne，一边按 supervise 的方式持 s.mu 写状态。
// 状态读写都在 s.mu 下完成，-race 不得报竞争；旧实现的锁外读会红。
func TestStatusAndRestartConcurrentWithStateWrites(t *testing.T) {
	s := New(t.TempDir(), "config.yaml", "")
	p := s.procs["gateway"]
	states := []ProcState{StateStopped, StateStarting, StateRunning, StateBackoff, StateFailed}

	stop := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(1)
	go func() { // 模拟 supervise 的状态写入：全程持 s.mu
		defer writers.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s.mu.Lock()
			p.state = states[i%len(states)]
			s.mu.Unlock()
		}
	}()

	var readers sync.WaitGroup
	readers.Add(3)
	go func() {
		defer readers.Done()
		for i := 0; i < 200; i++ {
			if got := s.Status(); len(got) != 2 {
				t.Errorf("Status 长度=%d, want 2", len(got))
				return
			}
		}
	}()
	go func() {
		defer readers.Done()
		for i := 0; i < 200; i++ {
			if err := s.StopOne("gateway"); err != nil {
				t.Errorf("StopOne: %v", err)
				return
			}
			_ = s.procState("gateway")
		}
	}()
	go func() {
		defer readers.Done()
		for i := 0; i < 5; i++ {
			// 状态被上面的写入方反复置为 stopped，等待循环很快退出；
			// 可执行文件不存在，launch 一定失败，错误内容不作为断言目标。
			_ = s.RestartOne("gateway")
		}
	}()

	readers.Wait()
	close(stop)
	writers.Wait()
}
