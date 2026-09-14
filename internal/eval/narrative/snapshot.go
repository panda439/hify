package narrative

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	SnapshotKindTruth      = "truth"
	SnapshotKindPrediction = "prediction"

	IdentityModeEndToEnd          = "end_to_end"
	IdentityModeGoldAliasAssisted = "gold_alias_assisted"
)

// Snapshot is a portable, immutable input to the deterministic evaluator.
// Raw responses and ledger content are intentionally stored outside this file;
// their content hashes make the report traceable without copying credentials or
// potentially large provider payloads into a score report.
type Snapshot struct {
	SchemaVersion           int        `json:"schema_version"`
	Kind                    string     `json:"kind"`
	Status                  string     `json:"status"`
	FrozenForAcceptance     bool       `json:"frozen_for_acceptance"`
	IdentityMode            string     `json:"identity_mode"`
	Relations               []Relation `json:"relations"`
	ModelConfigHash         string     `json:"model_config_hash,omitempty"`
	PriceVersion            string     `json:"price_version,omitempty"`
	RawResponseSnapshotPath string     `json:"raw_response_snapshot_path,omitempty"`
	LedgerSnapshotPath      string     `json:"ledger_snapshot_path,omitempty"`
	RawResponseSnapshotSHA  string     `json:"raw_response_snapshot_sha256,omitempty"`
	LedgerSnapshotSHA       string     `json:"ledger_snapshot_sha256,omitempty"`
}

// BindArtifactHashes reads optional raw-response and ledger snapshots and
// records their current hashes. The caller supplies the snapshot directory so
// relative references remain portable inside an experiment directory.
func BindArtifactHashes(snapshot Snapshot, snapshotDir string) (Snapshot, error) {
	var err error
	if snapshot.RawResponseSnapshotPath != "" {
		snapshot.RawResponseSnapshotSHA, err = fileHash(resolveArtifactPath(snapshotDir, snapshot.RawResponseSnapshotPath))
		if err != nil {
			return Snapshot{}, fmt.Errorf("hash raw response snapshot: %w", err)
		}
	}
	if snapshot.LedgerSnapshotPath != "" {
		snapshot.LedgerSnapshotSHA, err = fileHash(resolveArtifactPath(snapshotDir, snapshot.LedgerSnapshotPath))
		if err != nil {
			return Snapshot{}, fmt.Errorf("hash ledger snapshot: %w", err)
		}
	}
	return snapshot, nil
}

// InputProvenance is copied into a report, retaining only the identifiers a
// reviewer needs to recover the model configuration, raw outputs, and ledger.
type InputProvenance struct {
	ModelConfigHash        string `json:"model_config_hash,omitempty"`
	PriceVersion           string `json:"price_version,omitempty"`
	RawResponseSnapshotSHA string `json:"raw_response_snapshot_sha256,omitempty"`
	LedgerSnapshotSHA      string `json:"ledger_snapshot_sha256,omitempty"`
}

// Report is a reproducible metric calculation. Acceptance is true only when
// the supplied truth snapshot explicitly records frozen human gold.
type Report struct {
	SchemaVersion            int             `json:"schema_version"`
	Acceptance               bool            `json:"acceptance"`
	ReferenceOnly            bool            `json:"reference_only"`
	IdentityMode             string          `json:"identity_mode"`
	TruthSnapshotSHA256      string          `json:"truth_snapshot_sha256"`
	PredictionSnapshotSHA256 string          `json:"prediction_snapshot_sha256"`
	Truth                    InputProvenance `json:"truth"`
	Prediction               InputProvenance `json:"prediction"`
	Metrics                  Result          `json:"metrics"`
}

// ScoreSnapshots rejects anything other than a frozen human truth snapshot by
// default. allowReferenceOnly supports development checks, but makes the
// resulting report explicitly unusable as an acceptance metric.
func ScoreSnapshots(truth, prediction Snapshot, allowReferenceOnly bool) (Report, error) {
	if err := validateSnapshot(truth, SnapshotKindTruth); err != nil {
		return Report{}, fmt.Errorf("truth snapshot: %w", err)
	}
	if err := validateSnapshot(prediction, SnapshotKindPrediction); err != nil {
		return Report{}, fmt.Errorf("prediction snapshot: %w", err)
	}
	if truth.IdentityMode != prediction.IdentityMode {
		return Report{}, fmt.Errorf("identity modes differ: truth=%q prediction=%q", truth.IdentityMode, prediction.IdentityMode)
	}
	acceptance := truth.FrozenForAcceptance && truth.Status == "HUMAN_GOLD_FROZEN"
	if !acceptance && !allowReferenceOnly {
		return Report{}, fmt.Errorf("truth is not frozen human gold; use an explicitly reference-only run for development data")
	}

	truthHash, err := snapshotHash(truth)
	if err != nil {
		return Report{}, fmt.Errorf("hash truth snapshot: %w", err)
	}
	predictionHash, err := snapshotHash(prediction)
	if err != nil {
		return Report{}, fmt.Errorf("hash prediction snapshot: %w", err)
	}
	return Report{
		SchemaVersion:            1,
		Acceptance:               acceptance,
		ReferenceOnly:            !acceptance,
		IdentityMode:             truth.IdentityMode,
		TruthSnapshotSHA256:      truthHash,
		PredictionSnapshotSHA256: predictionHash,
		Truth:                    provenanceOf(truth),
		Prediction:               provenanceOf(prediction),
		Metrics:                  Score(truth.Relations, prediction.Relations),
	}, nil
}

func validateSnapshot(snapshot Snapshot, expectedKind string) error {
	if snapshot.SchemaVersion != 1 {
		return fmt.Errorf("unsupported schema version %d", snapshot.SchemaVersion)
	}
	if snapshot.Kind != expectedKind {
		return fmt.Errorf("kind=%q, want %q", snapshot.Kind, expectedKind)
	}
	if snapshot.IdentityMode != IdentityModeEndToEnd && snapshot.IdentityMode != IdentityModeGoldAliasAssisted {
		return fmt.Errorf("unsupported identity mode %q", snapshot.IdentityMode)
	}
	return nil
}

func snapshotHash(snapshot Snapshot) (string, error) {
	b, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:]), nil
}

func provenanceOf(snapshot Snapshot) InputProvenance {
	return InputProvenance{
		ModelConfigHash:        snapshot.ModelConfigHash,
		PriceVersion:           snapshot.PriceVersion,
		RawResponseSnapshotSHA: snapshot.RawResponseSnapshotSHA,
		LedgerSnapshotSHA:      snapshot.LedgerSnapshotSHA,
	}
}

func resolveArtifactPath(snapshotDir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(snapshotDir, path)
}

func fileHash(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:]), nil
}
