package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rf4server/internal/config"
	"rf4server/internal/logging"
)

// 默认口令与默认签名密钥必须在总览接口里可见：启动日志一闪而过，
// 用户真正会看的是管理后台首页。
func TestDashboardReportsWeakCredentialWarnings(t *testing.T) {
	db := testDB(t)
	weak := NewServer(config.Admin{
		JWTSecret: config.DefaultAdminJWTSecret,
		Username:  config.DefaultAdminUsername,
		Password:  config.DefaultAdminPassword,
	}, db, logging.Discard(), nil)

	rec := httptest.NewRecorder()
	weak.handleDashboard(rec, httptest.NewRequest(http.MethodGet, "/api/dashboard", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("总览接口状态 = %d，期望 200: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析总览响应失败: %v，响应体: %s", err, rec.Body.String())
	}
	if len(got.Warnings) != 2 {
		t.Fatalf("默认密码与默认密钥应各报一条告警，实得 %d 条: %v", len(got.Warnings), got.Warnings)
	}
	joined := strings.Join(got.Warnings, "\n")
	for _, want := range []string{"admin.password", "admin.jwt_secret"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("告警应点名配置项 %q，实得: %s", want, joined)
		}
	}

	strong := NewServer(config.Admin{
		JWTSecret: "lan-secret",
		Username:  "admin",
		Password:  "strong-pass",
	}, db, logging.Discard(), nil)
	rec = httptest.NewRecorder()
	strong.handleDashboard(rec, httptest.NewRequest(http.MethodGet, "/api/dashboard", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("总览接口状态 = %d，期望 200: %s", rec.Code, rec.Body.String())
	}
	got.Warnings = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析总览响应失败: %v，响应体: %s", err, rec.Body.String())
	}
	if len(got.Warnings) != 0 {
		t.Fatalf("自定义凭据不应告警，实得: %v", got.Warnings)
	}
}

// 总览告警必须反映实际写入 admin_user 的凭据，而不是只看配置原文。
func TestDashboardWarningsUseEffectiveCredentials(t *testing.T) {
	srv := NewServer(config.Admin{
		JWTSecret: "lan-secret",
		Username:  "",
		Password:  "strong-pass",
	}, testDB(t), logging.Discard(), nil)
	rec := httptest.NewRecorder()
	srv.handleDashboard(rec, httptest.NewRequest(http.MethodGet, "/api/dashboard", nil))
	var got struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析总览响应失败: %v，响应体: %s", err, rec.Body.String())
	}
	if len(got.Warnings) != 1 {
		t.Fatalf("空用户名回退默认密码应报一条告警，实得 %d 条: %v", len(got.Warnings), got.Warnings)
	}
	if joined := strings.Join(got.Warnings, "\n"); !strings.Contains(joined, config.DefaultAdminPassword) || !strings.Contains(joined, "admin.username") {
		t.Fatalf("告警应点名默认密码和 admin.username，实得: %s", joined)
	}
}

// 前端按数组渲染告警条：没有告警时也必须是 []，不能是 null。
func TestDashboardWarningsStayArrayWithoutDefaults(t *testing.T) {
	srv := NewServer(config.Admin{JWTSecret: "lan-secret", Username: "admin", Password: "strong-pass"},
		testDB(t), logging.Discard(), nil)
	rec := httptest.NewRecorder()
	srv.handleDashboard(rec, httptest.NewRequest(http.MethodGet, "/api/dashboard", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("总览接口状态 = %d，期望 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"warnings":[]`) {
		t.Fatalf("自定义凭据下 warnings 应为空数组，实得: %s", rec.Body.String())
	}
}
