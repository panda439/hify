package provider

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// 这个文件存在的唯一原因：`addModelRequest.Capability` 的 gin binding 标签
// 是能力白名单的**第三处**，而它在请求进到 handler 逻辑之前就生效。
//
// 001-rag-query-rerank 加 `rerank` 能力时，`service.go` 和 `handler.go` 两处
// 显式判断都改对了，唯独漏了这个标签——结果是 rerank 模型**根本注册不进去**
// （HTTP 层直接 400），而这是它唯一的注册入口（前端的能力下拉暂时没有这个
// 选项）。整套单元测试和集成测试全绿，因为它们都是直接调 service/handler，
// 绕过了 gin 的 binding；这个缺陷是靠 /smoke-test 用真实 HTTP 请求打出来的。
//
// 所以这里补一组**只测 binding 标签本身**的测试：不连数据库、不构造 service，
// 只把 JSON 喂给 ShouldBindJSON，断言哪些 capability 值能过、哪些不能。
// 以后再加第四种能力时，这组测试会在忘记改标签的那一刻就红。

func bindAddModelCapability(t *testing.T, capability string) error {
	t.Helper()
	gin.SetMode(gin.TestMode)

	body := `{"model_name":"m","capability":"` + capability + `"}`
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	var req addModelRequest
	return c.ShouldBindJSON(&req)
}

func TestAddModelRequestBindingAcceptsEveryDeclaredCapability(t *testing.T) {
	// 白名单必须与 model.go 的 Capability* 常量完全一致——常量加了、标签
	// 没加，就是那个 smoke test 抓到的缺陷本身。
	for _, capability := range []string{CapabilityChat, CapabilityEmbedding, CapabilityRerank} {
		t.Run(capability, func(t *testing.T) {
			if err := bindAddModelCapability(t, capability); err != nil {
				t.Fatalf("capability %q must pass gin binding, got: %v —— 能力常量已声明但 dto.go 的 binding 标签没同步，HTTP 层会在 handler 之前就 400", capability, err)
			}
		})
	}
}

func TestAddModelRequestBindingRejectsUnknownCapability(t *testing.T) {
	// 常见的手误拼写；标签放行了它们才是真正的问题（会一路写进数据库，
	// 然后撞上 provider_models 的 CHECK 约束报一个难懂的 500）。
	for _, capability := range []string{"reranker", "rerank_model", "Chat", "embeddings", ""} {
		t.Run("reject_"+capability, func(t *testing.T) {
			if err := bindAddModelCapability(t, capability); err == nil {
				t.Fatalf("capability %q must be rejected by gin binding", capability)
			}
		})
	}
}
