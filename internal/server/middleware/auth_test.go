package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"hify/internal/platform/jwt"
)

// 链路 8（认证与权限门禁）：RequireAuth / RequireRole 的失败模式是
// "静默放行"，必须有拒绝用例兜底。不打 DB——JWT 校验本来就不查库。

const testSecret = "test-secret-at-least-32-bytes-long!"

// user.RoleAdmin 的字面量副本——user 包依赖 middleware，测试里反向引用会造成
// import cycle。若角色常量改名，TestRequireRole 会因签发的 token 角色对不上
// 而失败，不会静默漂移。
const roleAdmin = "admin"

func newAuthRouter(role string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api")
	g.Use(RequireAuth(testSecret))
	g.GET("/me", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"uid": UserIDFrom(c), "role": RoleFrom(c)})
	})
	admin := g.Group("/admin")
	admin.Use(RequireRole(role))
	admin.GET("/panel", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

func issueToken(t *testing.T, secret, uid, role string, ttl time.Duration) string {
	t.Helper()
	token, err := jwt.Issue(secret, jwt.Claims{UserID: uid, Role: role}, ttl)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return token
}

func doGet(r *gin.Engine, path, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRequireAuthRejections(t *testing.T) {
	r := newAuthRouter(roleAdmin)
	valid := issueToken(t, testSecret, "u1", "member", time.Minute)
	wrongSecret := issueToken(t, "another-secret-also-32-bytes-long!!", "u1", "member", time.Minute)
	expired := issueToken(t, testSecret, "u1", "member", -time.Minute)

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"not bearer scheme", "Basic abc", http.StatusUnauthorized},
		{"bearer with empty token", "Bearer ", http.StatusUnauthorized},
		{"garbage token", "Bearer not-a-jwt", http.StatusUnauthorized},
		{"token signed with wrong secret", "Bearer " + wrongSecret, http.StatusUnauthorized},
		{"expired token", "Bearer " + expired, http.StatusUnauthorized},
		{"valid token", "Bearer " + valid, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if w := doGet(r, "/api/me", tc.header); w.Code != tc.want {
				t.Fatalf("GET /api/me with %q = %d, want %d (body %s)", tc.name, w.Code, tc.want, w.Body)
			}
		})
	}
}

func TestRequireAuthPropagatesClaims(t *testing.T) {
	r := newAuthRouter(roleAdmin)
	token := issueToken(t, testSecret, "user-42", roleAdmin, time.Minute)
	w := doGet(r, "/api/me", "Bearer "+token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`"uid":"user-42"`, `"role":"admin"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %s missing %s", body, want)
		}
	}
}

func TestRequireRole(t *testing.T) {
	r := newAuthRouter(roleAdmin)
	memberToken := issueToken(t, testSecret, "u1", "member", time.Minute)
	adminToken := issueToken(t, testSecret, "u2", roleAdmin, time.Minute)

	// member 打 admin 路由：403（不是 401——已认证但无权限）。
	if w := doGet(r, "/api/admin/panel", "Bearer "+memberToken); w.Code != http.StatusForbidden {
		t.Fatalf("member on admin route = %d, want 403", w.Code)
	}
	if w := doGet(r, "/api/admin/panel", "Bearer "+adminToken); w.Code != http.StatusOK {
		t.Fatalf("admin on admin route = %d, want 200", w.Code)
	}
	// 未认证直接打 admin 路由：仍是 401（RequireAuth 先拦）。
	if w := doGet(r, "/api/admin/panel", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous on admin route = %d, want 401", w.Code)
	}
}
