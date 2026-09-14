package knowledge

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"hify/internal/provider"
)

// TestConfiguredProviderNarrativePrecheck validates the exact production
// ChatOnce path with one real corpus fragment before a paid full-book run.
// It is opt-in because it consumes the provider account configured by Hify.
func TestConfiguredProviderNarrativePrecheck(t *testing.T) {
	modelID := strings.TrimSpace(os.Getenv("HIFY_NARRATIVE_PRECHECK_PROVIDER_MODEL_ID"))
	if modelID == "" {
		t.Skip("HIFY_NARRATIVE_PRECHECK_PROVIDER_MODEL_ID 未设置，跳过真实供应商预检")
	}
	model, label := fullBookConfiguredProvider(t, modelID)
	root := fullBookRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "eval/corpus/aq-010/ch01.txt"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)
	if len([]rune(content)) > 500 {
		content = string([]rune(content)[:500])
	}
	prompt := buildExtractInstruction() + "\n\n原文片段：\n" + content
	maxTokens := maxOutputTokens
	if raw := strings.TrimSpace(os.Getenv("HIFY_NARRATIVE_PRECHECK_MAX_OUTPUT_TOKENS")); raw != "" {
		var err error
		maxTokens, err = strconv.Atoi(raw)
		if err != nil || maxTokens <= 0 {
			t.Fatalf("HIFY_NARRATIVE_PRECHECK_MAX_OUTPUT_TOKENS must be positive integer, got %q", raw)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	out, err := model.ChatOnce(ctx, modelID, provider.ChatRequest{
		Messages:  []provider.Message{{Role: "user", Content: prompt}},
		MaxTokens: maxTokens,
		JSONMode:  true,
	}, 90*time.Second)
	if err != nil {
		t.Fatalf("%s ChatOnce: %v", label, err)
	}
	if out.Outcome != provider.AttemptCompleted {
		t.Fatalf("%s outcome=%s code=%s response=%s", label, out.Outcome, out.ErrorCode, out.Message.Content)
	}
	resp, err := parseExtractionResponse([]byte(out.Message.Content))
	if err != nil {
		t.Fatalf("%s returned invalid extraction JSON: %v; response=%s", label, err, out.Message.Content)
	}
	if _, err := resolveExtraction(extractionChunkView{ChunkID: "aq-ch01-precheck", DocumentVersion: 1, Content: content}, resp); err != nil {
		t.Fatalf("%s response failed evidence validation: %v; response=%s", label, err, out.Message.Content)
	}
	t.Logf("%s JSON/evidence precheck passed in %dms", label, out.ElapsedMs)
}
