package provider

import "testing"

func TestChatOnceRequestUsesResolvedModelName(t *testing.T) {
	request := chatOnceRequest(Model{ID: "model-id", ModelName: "deepseek-v4-flash"}, ChatRequest{Model: "wrong", MaxTokens: 256})
	if request.Model != "deepseek-v4-flash" {
		t.Fatalf("request model = %q, want resolved model name", request.Model)
	}
}
