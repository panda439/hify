package provider

import (
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

func TestToOpenAIRequestJSONMode(t *testing.T) {
	t.Run("requested", func(t *testing.T) {
		got := toOpenAIRequest(ChatRequest{Model: "deepseek-v4-pro", JSONMode: true}, false)
		if got.ResponseFormat == nil || got.ResponseFormat.Type != openai.ChatCompletionResponseFormatTypeJSONObject {
			t.Fatalf("response_format = %#v, want json_object", got.ResponseFormat)
		}
	})
	t.Run("not requested", func(t *testing.T) {
		got := toOpenAIRequest(ChatRequest{Model: "ordinary-chat"}, false)
		if got.ResponseFormat != nil {
			t.Fatalf("response_format = %#v, want nil", got.ResponseFormat)
		}
	})
}
