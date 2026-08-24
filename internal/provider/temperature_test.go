package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// T023a：go-openai v1.41.2 的 ChatCompletionRequest.Temperature 是
// `json:"temperature,omitempty"` 的 float32——显式 0 和"没传"在它眼里长得
// 一模一样，序列化时会被整个省略，供应商按自己的默认温度（通常 1.0）执
// 行，且完全没有报错。这条测试直接断言"真正发到线上的请求体字节"，不是
// 断言 Go 结构体字段值——只测结构体字段的话，即便 Temperature 改成
// *float64，Chat 内部仍然是把值喂给 go-openai 的
// openai.ChatCompletionRequest.Temperature（float32，同样的 omitempty
// 标签），测试会在"看起来修好了"但实际字节没变的情况下通过，等于白修。

func ptrFloat64(v float64) *float64 { return &v }

func newTestChatClient(t *testing.T, capture func(body []byte)) *openAICompatClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		capture(body)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	}))
	t.Cleanup(srv.Close)
	return newOpenAICompatClient(srv.URL, "test-key", nil, srv.Client())
}

func TestChatRequestBodyIncludesExplicitZeroTemperature(t *testing.T) {
	var captured map[string]any
	client := newTestChatClient(t, func(body []byte) {
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("decode captured request body: %v (raw: %s)", err, body)
		}
	})

	_, err := client.Chat(context.Background(), ChatRequest{
		Model:       "m",
		Messages:    []Message{{Role: RoleUser, Content: "hi"}},
		Temperature: ptrFloat64(0),
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	got, ok := captured["temperature"]
	if !ok {
		t.Fatalf("request body missing \"temperature\" field entirely, want it present as 0: %v", captured)
	}
	if got != float64(0) {
		t.Fatalf("temperature = %v, want 0", got)
	}
}

func TestChatRequestBodyOmitsTemperatureWhenUnset(t *testing.T) {
	var captured map[string]any
	client := newTestChatClient(t, func(body []byte) {
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("decode captured request body: %v (raw: %s)", err, body)
		}
	})

	_, err := client.Chat(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		// Temperature 留 nil：代表调用方没有意见，应该让供应商用它自己的
		// 默认值——这时"省略字段"才是正确行为，不能替调用方伪造一个 0。
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if _, ok := captured["temperature"]; ok {
		t.Fatalf("request body has \"temperature\" field, want it omitted when Temperature is nil: %v", captured)
	}
}

func TestChatRequestBodyIncludesExplicitNonZeroTemperature(t *testing.T) {
	var captured map[string]any
	client := newTestChatClient(t, func(body []byte) {
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("decode captured request body: %v (raw: %s)", err, body)
		}
	})

	_, err := client.Chat(context.Background(), ChatRequest{
		Model:       "m",
		Messages:    []Message{{Role: RoleUser, Content: "hi"}},
		Temperature: ptrFloat64(0.7),
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := captured["temperature"]; got != 0.7 {
		t.Fatalf("temperature = %v, want 0.7", got)
	}
}

// 防回归：请求体里带一个参数名叫 temperature 的工具时，顶层 temperature
// 仍然必须被补进去。review 时发现原实现用 bytes.Contains 扫整个 body，
// 会把工具参数里的 "temperature" 误判成"顶层已经有了"而跳过补丁——那正好
// 悄悄退回 T023a 要修的 bug，且只在挂了这类工具的 Agent 上复现。
func TestInjectZeroTemperatureIgnoresToolParameterNamedTemperature(t *testing.T) {
	body := []byte(`{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"properties":{"temperature":{"type":"number"}}}}}]}`)

	patched, err := injectZeroTemperature(body)
	if err != nil {
		t.Fatalf("injectZeroTemperature: %v", err)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(patched, &top); err != nil {
		t.Fatalf("patched body is not valid json: %v", err)
	}
	raw, ok := top["temperature"]
	if !ok {
		t.Fatal("top-level temperature missing: the tool parameter named temperature was mistaken for it")
	}
	if string(raw) != "0" {
		t.Fatalf("top-level temperature = %s, want 0", raw)
	}
}

// 顶层已经有 temperature 时不重复注入（补丁必须是幂等的）。
func TestInjectZeroTemperatureLeavesExistingTopLevelKeyAlone(t *testing.T) {
	body := []byte(`{"model":"m","temperature":0.7,"messages":[]}`)

	patched, err := injectZeroTemperature(body)
	if err != nil {
		t.Fatalf("injectZeroTemperature: %v", err)
	}
	if string(patched) != string(body) {
		t.Fatalf("body was modified: %s", patched)
	}
}
