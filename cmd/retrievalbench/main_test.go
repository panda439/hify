package main

import (
	"encoding/json"
	"strings"
	"testing"

	"hify/internal/eval/retrievalbench"
)

func TestParseCommandRequiresSubcommandAndAcceptsScore(t *testing.T) {
	if _, err := parseArgs([]string{"retrievalbench"}); err == nil {
		t.Fatal("expected subcommand error")
	}
	cfg, err := parseArgs([]string{"retrievalbench", "score", "--input", "run.json", "--output", "report.json"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.command != "score" || cfg.input != "run.json" || cfg.output != "report.json" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestBenchmarkFingerprintCapturesCompatibilityAndRedactsService(t *testing.T) {
	ds := retrievalbench.PreparedDataset{Manifest: retrievalbench.DatasetManifest{
		Dataset: "miracl", Revision: "v1.0", ConfigSHA256: "selection-sha",
		QueryIDs: []string{"q1"}, DocumentIDs: []string{"d1"},
		QrelsSHA256: "qrels-sha", CorpusSHA256: "corpus-sha",
	}}
	fp := buildBenchmarkFingerprint(ds, "bge-m3:567m", "digest", "http://127.0.0.1:11434", "{\"size\":500}", "{\"top_k\":10}")
	if fp.SelectionConfigSHA256 != "selection-sha" || fp.ChunkConfig == "" || fp.RetrievalConfig == "" || fp.ServiceEndpointID == "" {
		t.Fatalf("compatibility fingerprint omitted actual configuration: %+v", fp)
	}
	b, err := json.Marshal(fp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "127.0.0.1") || strings.Contains(string(b), "11434") {
		t.Fatalf("fingerprint leaked service address: %s", b)
	}
}
