package narrative

import "testing"

func TestScoreSnapshotsRequiresFrozenHumanTruth(t *testing.T) {
	truth := Snapshot{
		SchemaVersion: 1,
		Kind:          SnapshotKindTruth,
		Status:        "AI_DRAFT_NOT_HUMAN_GOLD",
		IdentityMode:  IdentityModeEndToEnd,
		Relations:     []Relation{{Chapter: 1, Subject: "a", Object: "b", Type: "认识", Directed: false}},
	}
	prediction := Snapshot{
		SchemaVersion: 1,
		Kind:          SnapshotKindPrediction,
		Status:        "COMPLETED",
		IdentityMode:  IdentityModeEndToEnd,
		Relations:     []Relation{{Chapter: 1, Subject: "a", Object: "b", Type: "认识", Directed: false}},
	}

	if _, err := ScoreSnapshots(truth, prediction, false); err == nil {
		t.Fatal("unfrozen AI draft truth must be rejected")
	}

	report, err := ScoreSnapshots(truth, prediction, true)
	if err != nil {
		t.Fatalf("reference-only score: %v", err)
	}
	if report.Acceptance || !report.ReferenceOnly {
		t.Fatalf("report = %+v, want reference-only", report)
	}
}

func TestScoreSnapshotsReportsIdentityModeAndInputHashes(t *testing.T) {
	truth := Snapshot{
		SchemaVersion:          1,
		Kind:                   SnapshotKindTruth,
		Status:                 "HUMAN_GOLD_FROZEN",
		FrozenForAcceptance:    true,
		IdentityMode:           IdentityModeGoldAliasAssisted,
		Relations:              []Relation{{Chapter: 2, Subject: "a", Object: "b", Type: "亲属", Directed: false, EvidenceIDs: []string{"C02P003"}}},
		ModelConfigHash:        "truth-config-hash",
		PriceVersion:           "not-applicable",
		RawResponseSnapshotSHA: "truth-raw-hash",
	}
	prediction := Snapshot{
		SchemaVersion:          1,
		Kind:                   SnapshotKindPrediction,
		Status:                 "COMPLETED",
		IdentityMode:           IdentityModeGoldAliasAssisted,
		Relations:              []Relation{{Chapter: 2, Subject: "b", Object: "a", Type: "亲属", Directed: false, EvidenceIDs: []string{"C02P003"}}},
		ModelConfigHash:        "model-config-hash",
		PriceVersion:           "ollama-local",
		RawResponseSnapshotSHA: "raw-hash",
		LedgerSnapshotSHA:      "ledger-hash",
	}

	report, err := ScoreSnapshots(truth, prediction, false)
	if err != nil {
		t.Fatalf("ScoreSnapshots: %v", err)
	}
	if !report.Acceptance || report.ReferenceOnly {
		t.Fatalf("report acceptance = %+v", report)
	}
	if report.IdentityMode != IdentityModeGoldAliasAssisted || report.Metrics.TruePositives != 1 {
		t.Fatalf("report = %+v", report)
	}
	if report.TruthSnapshotSHA256 == "" || report.PredictionSnapshotSHA256 == "" {
		t.Fatalf("missing reproducibility hashes: %+v", report)
	}
	if report.Prediction.ModelConfigHash != "model-config-hash" || report.Prediction.LedgerSnapshotSHA != "ledger-hash" {
		t.Fatalf("prediction provenance = %+v", report.Prediction)
	}
}

func TestScoreSnapshotsRejectsDifferentIdentityModes(t *testing.T) {
	truth := frozenTruth(IdentityModeEndToEnd)
	prediction := completedPrediction(IdentityModeGoldAliasAssisted)
	if _, err := ScoreSnapshots(truth, prediction, false); err == nil {
		t.Fatal("mixed identity modes must be rejected")
	}
}

func frozenTruth(mode string) Snapshot {
	return Snapshot{SchemaVersion: 1, Kind: SnapshotKindTruth, Status: "HUMAN_GOLD_FROZEN", FrozenForAcceptance: true, IdentityMode: mode}
}

func completedPrediction(mode string) Snapshot {
	return Snapshot{SchemaVersion: 1, Kind: SnapshotKindPrediction, Status: "COMPLETED", IdentityMode: mode}
}
