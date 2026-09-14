package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hify/internal/eval/narrative"
)

func TestRunWritesReferenceOnlyReportForExplicitAIReference(t *testing.T) {
	dir := t.TempDir()
	truthPath := filepath.Join(dir, "truth.json")
	predictionPath := filepath.Join(dir, "prediction.json")
	outPath := filepath.Join(dir, "report.json")
	writeSnapshot(t, truthPath, narrative.Snapshot{
		SchemaVersion: 1, Kind: narrative.SnapshotKindTruth, Status: "AI_DRAFT_NOT_HUMAN_GOLD",
		IdentityMode: narrative.IdentityModeEndToEnd,
		Relations:    []narrative.Relation{{Chapter: 1, Subject: "a", Object: "b", Type: "认识", Directed: false}},
	})
	writeSnapshot(t, predictionPath, narrative.Snapshot{
		SchemaVersion: 1, Kind: narrative.SnapshotKindPrediction, Status: "COMPLETED",
		IdentityMode:            narrative.IdentityModeEndToEnd,
		Relations:               []narrative.Relation{{Chapter: 1, Subject: "a", Object: "b", Type: "认识", Directed: false}},
		RawResponseSnapshotPath: "raw.jsonl",
		LedgerSnapshotPath:      "ledger.json",
	})
	if err := os.WriteFile(filepath.Join(dir, "raw.jsonl"), []byte("raw response\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ledger.json"), []byte("{\"usage\":\"unknown\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := run(truthPath, predictionPath, outPath, true); err != nil {
		t.Fatalf("run: %v", err)
	}
	report, err := loadReport(outPath)
	if err != nil {
		t.Fatalf("load report: %v", err)
	}
	if !report.ReferenceOnly || report.Acceptance || report.Metrics.TruePositives != 1 {
		t.Fatalf("report = %+v", report)
	}
	if len(report.Prediction.RawResponseSnapshotSHA) != 64 || len(report.Prediction.LedgerSnapshotSHA) != 64 {
		t.Fatalf("report did not hash raw inputs: %+v", report.Prediction)
	}
}

func TestRunRejectsMissingRawArtifact(t *testing.T) {
	dir := t.TempDir()
	truthPath := filepath.Join(dir, "truth.json")
	predictionPath := filepath.Join(dir, "prediction.json")
	writeSnapshot(t, truthPath, narrative.Snapshot{SchemaVersion: 1, Kind: narrative.SnapshotKindTruth, Status: "HUMAN_GOLD_FROZEN", FrozenForAcceptance: true, IdentityMode: narrative.IdentityModeEndToEnd})
	writeSnapshot(t, predictionPath, narrative.Snapshot{SchemaVersion: 1, Kind: narrative.SnapshotKindPrediction, Status: "COMPLETED", IdentityMode: narrative.IdentityModeEndToEnd, RawResponseSnapshotPath: "missing.jsonl"})

	err := run(truthPath, predictionPath, filepath.Join(dir, "report.json"), false)
	if err == nil || !strings.Contains(err.Error(), "raw response") {
		t.Fatalf("err = %v, want missing raw artifact error", err)
	}
}

func TestRunRejectsUnfrozenTruthWithoutReferenceOnlyFlag(t *testing.T) {
	dir := t.TempDir()
	truthPath := filepath.Join(dir, "truth.json")
	predictionPath := filepath.Join(dir, "prediction.json")
	writeSnapshot(t, truthPath, narrative.Snapshot{SchemaVersion: 1, Kind: narrative.SnapshotKindTruth, Status: "AI_DRAFT_NOT_HUMAN_GOLD", IdentityMode: narrative.IdentityModeEndToEnd})
	writeSnapshot(t, predictionPath, narrative.Snapshot{SchemaVersion: 1, Kind: narrative.SnapshotKindPrediction, Status: "COMPLETED", IdentityMode: narrative.IdentityModeEndToEnd})

	if err := run(truthPath, predictionPath, filepath.Join(dir, "report.json"), false); err == nil {
		t.Fatal("expected unfrozen truth to be rejected")
	}
}

func writeSnapshot(t *testing.T, path string, snapshot narrative.Snapshot) {
	t.Helper()
	if err := saveSnapshot(path, snapshot); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	os.Exit(code)
}
