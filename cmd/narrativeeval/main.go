// Command narrativeeval recomputes narrative extraction metrics from saved
// snapshots. It never calls a model or an LLM judge: acceptance metrics require
// a separately frozen human-truth snapshot.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"hify/internal/eval/narrative"
)

func main() {
	truthPath := flag.String("truth", "", "frozen truth snapshot JSON")
	predictionPath := flag.String("predictions", "", "model prediction snapshot JSON")
	outPath := flag.String("out", "", "report JSON output path")
	allowReferenceOnly := flag.Bool("allow-reference-only", false, "allow an unfrozen development reference; report is marked non-acceptance")
	flag.Parse()

	if *truthPath == "" || *predictionPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "narrativeeval: --truth, --predictions, and --out are required")
		os.Exit(2)
	}
	if err := run(*truthPath, *predictionPath, *outPath, *allowReferenceOnly); err != nil {
		fmt.Fprintln(os.Stderr, "narrativeeval:", err)
		os.Exit(1)
	}
}

func run(truthPath, predictionPath, outPath string, allowReferenceOnly bool) error {
	truth, err := loadSnapshot(truthPath)
	if err != nil {
		return fmt.Errorf("read truth snapshot: %w", err)
	}
	prediction, err := loadSnapshot(predictionPath)
	if err != nil {
		return fmt.Errorf("read prediction snapshot: %w", err)
	}
	report, err := narrative.ScoreSnapshots(truth, prediction, allowReferenceOnly)
	if err != nil {
		return err
	}
	if err := saveReport(outPath, report); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	if report.ReferenceOnly {
		fmt.Printf("参考性结果已写入 %s（非人工真值验收）\n", outPath)
		return nil
	}
	fmt.Printf("验收指标已写入 %s\n", outPath)
	return nil
}

func loadSnapshot(path string) (narrative.Snapshot, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return narrative.Snapshot{}, err
	}
	var snapshot narrative.Snapshot
	if err := json.Unmarshal(b, &snapshot); err != nil {
		return narrative.Snapshot{}, err
	}
	return narrative.BindArtifactHashes(snapshot, filepath.Dir(path))
}

func saveSnapshot(path string, snapshot narrative.Snapshot) error {
	return saveJSON(path, snapshot)
}

func loadReport(path string) (narrative.Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return narrative.Report{}, err
	}
	var report narrative.Report
	if err := json.Unmarshal(b, &report); err != nil {
		return narrative.Report{}, err
	}
	return report, nil
}

func saveReport(path string, report narrative.Report) error {
	return saveJSON(path, report)
}

func saveJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
