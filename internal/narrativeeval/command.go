package narrativeeval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type ScoreOutput struct {
	SchemaVersion    int       `json:"schema_version"`
	ReferenceKind    string    `json:"reference_kind"`
	TruthSHA256      string    `json:"truth_sha256"`
	PredictionSHA256 string    `json:"prediction_sha256"`
	AliasesSHA256    string    `json:"aliases_sha256"`
	Model            ModelInfo `json:"model"`
	Metrics          Report    `json:"metrics"`
}

func RunScore(truthPath, predictionPath, aliasesPath string, out io.Writer) error {
	truth, err := LoadRelationsJSONL(truthPath)
	if err != nil {
		return fmt.Errorf("load reference: %w", err)
	}
	snapshot, err := LoadSnapshot(predictionPath)
	if err != nil {
		return fmt.Errorf("load prediction: %w", err)
	}
	aliases, err := LoadAliases(aliasesPath)
	if err != nil {
		return fmt.Errorf("load aliases: %w", err)
	}

	truthHash, err := fileSHA256(truthPath)
	if err != nil {
		return err
	}
	predHash, err := fileSHA256(predictionPath)
	if err != nil {
		return err
	}
	aliasHash, err := fileSHA256(aliasesPath)
	if err != nil {
		return err
	}

	result := ScoreOutput{
		SchemaVersion: 1, ReferenceKind: snapshot.ReferenceKind,
		TruthSHA256: truthHash, PredictionSHA256: predHash, AliasesSHA256: aliasHash,
		Model: snapshot.Model, Metrics: Score(truth, snapshot.Relations, aliases),
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

func fileSHA256(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
