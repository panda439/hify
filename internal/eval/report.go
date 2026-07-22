package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Save writes report as JSON to path, creating parent directories as
// needed (eval/runs/ won't exist on a fresh checkout).
func Save(report RunReport, path string) error {
	if report.RanAt == "" {
		report.RanAt = time.Now().UTC().Format(time.RFC3339)
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("eval: marshal report: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("eval: create report dir: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("eval: write report: %w", err)
	}
	return nil
}

// Load reads back a report written by Save.
func Load(path string) (RunReport, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return RunReport{}, fmt.Errorf("eval: read report: %w", err)
	}
	var report RunReport
	if err := json.Unmarshal(b, &report); err != nil {
		return RunReport{}, fmt.Errorf("eval: unmarshal report: %w", err)
	}
	return report, nil
}

// Compare renders a Markdown regression report: current vs baseline score
// per case, with a dedicated "退步" section calling out anything that
// dropped — that section is the whole point of running this against a
// baseline instead of just reading the current run in isolation.
func Compare(current, baseline RunReport) string {
	baseByName := make(map[string]CaseResult, len(baseline.Results))
	for _, r := range baseline.Results {
		baseByName[r.Name] = r
	}

	var sb strings.Builder
	sb.WriteString("# Eval 回归报告\n\n")
	fmt.Fprintf(&sb, "本次运行：%s　对比基线：%s\n\n", current.RanAt, baseline.RanAt)
	sb.WriteString("| Case | 本次分数 | 基线分数 | 变化 | 备注 |\n")
	sb.WriteString("|---|---|---|---|---|\n")

	var regressed []string
	for _, r := range current.Results {
		base, hasBase := baseByName[r.Name]
		note := ""
		if r.Err != "" {
			note = "运行失败：" + r.Err
		}

		baseCell := "—"
		deltaCell := "—"
		if hasBase {
			baseCell = fmt.Sprintf("%d", base.Score)
			delta := r.Score - base.Score
			deltaCell = fmt.Sprintf("%+d", delta)
			if delta < 0 {
				regressed = append(regressed, fmt.Sprintf("- **%s**：%d → %d（%s）", r.Name, base.Score, r.Score, r.Reasoning))
			}
		}
		fmt.Fprintf(&sb, "| %s | %d | %s | %s | %s |\n", r.Name, r.Score, baseCell, deltaCell, note)
	}

	sb.WriteString("\n## 退步的 case\n\n")
	if len(regressed) == 0 {
		sb.WriteString("无。\n")
	} else {
		for _, line := range regressed {
			sb.WriteString(line + "\n")
		}
	}
	return sb.String()
}

// Regressed reports whether any case's score dropped by at least
// threshold points versus baseline — cmd/evalrunner uses this to decide
// its exit code, so a regression can gate CI without a human reading the
// Markdown output every time.
func Regressed(current, baseline RunReport, threshold int) bool {
	baseByName := make(map[string]CaseResult, len(baseline.Results))
	for _, r := range baseline.Results {
		baseByName[r.Name] = r
	}
	for _, r := range current.Results {
		if base, ok := baseByName[r.Name]; ok && base.Score-r.Score >= threshold {
			return true
		}
	}
	return false
}
