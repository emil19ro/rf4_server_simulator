package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"rf4server/internal/supervisor"
)

// name 直接来自 query，未知进程名应回 404 而不是 500，面板才能当成参数错误提示。
func TestActionHandlerUnknownProcessReturns404(t *testing.T) {
	a := &app{sup: supervisor.New(t.TempDir(), "config.yaml", "")}
	for _, action := range []string{"start", "stop", "restart"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/"+action+"?name=不存在的进程", nil)
		a.actionHandler(action)(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s 未知进程 code=%d, want 404; body=%s", action, rr.Code, rr.Body.String())
		}
	}
}
