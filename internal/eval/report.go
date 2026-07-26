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
// baseline instead of just reading the current run in isolation. A second
// table shows the deterministic RAG metrics (Hit/MRR/CitationRequirement/
// ExpectedDocumentCited) side by side with their baseline values — see
// Regressed's doc comment for why these don't (yet) affect the exit code.
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

	sb.WriteString("\n## 检索 / 引用指标\n\n")
	sb.WriteString("“—”表示该 case 未配置对应字段（expected_document_ids / require_citation），不代表指标为 false/0——旧版 baseline（跑在这次改造之前）里所有 case 在这张表都会显示“—”，属于正常的“基线缺少这些字段”情形，不是数据异常。\n\n")
	sb.WriteString("| Case | RetrievalHit 本次/基线/变化 | MRR 本次/基线/变化 | CitationRequirementMet 本次/基线/变化 | ExpectedDocumentCited 本次/基线/变化 |\n")
	sb.WriteString("|---|---|---|---|---|\n")
	for _, r := range current.Results {
		base := baseByName[r.Name] // zero value (all Evaluated=false) when absent — renders as "—/—/—"
		fmt.Fprintf(&sb, "| %s | %s | %s | %s | %s |\n",
			r.Name,
			boolMetricTriple(r.Metrics.RetrievalHit, base.Metrics.RetrievalHit),
			floatMetricTriple(r.Metrics.MRR, base.Metrics.MRR),
			boolMetricTriple(r.Metrics.CitationRequirementMet, base.Metrics.CitationRequirementMet),
			boolMetricTriple(r.Metrics.ExpectedDocumentCited, base.Metrics.ExpectedDocumentCited),
		)
	}

	return sb.String()
}

func boolMetricCell(m BoolMetric) string {
	if !m.Evaluated {
		return "—"
	}
	if m.Value {
		return "true"
	}
	return "false"
}

func floatMetricCell(m FloatMetric) string {
	if !m.Evaluated {
		return "—"
	}
	return fmt.Sprintf("%.2f", m.Value)
}

func boolMetricTriple(cur, base BoolMetric) string {
	delta := "—"
	if cur.Evaluated && base.Evaluated {
		switch {
		case cur.Value == base.Value:
			delta = "="
		case cur.Value:
			delta = "+"
		default:
			delta = "-"
		}
	}
	return fmt.Sprintf("%s / %s / %s", boolMetricCell(cur), boolMetricCell(base), delta)
}

func floatMetricTriple(cur, base FloatMetric) string {
	delta := "—"
	if cur.Evaluated && base.Evaluated {
		delta = fmt.Sprintf("%+.2f", cur.Value-base.Value)
	}
	return fmt.Sprintf("%s / %s / %s", floatMetricCell(cur), floatMetricCell(base), delta)
}

// Regressed reports whether any case's score dropped by at least
// threshold points versus baseline — cmd/evalrunner uses this to decide
// its exit code, so a regression can gate CI without a human reading the
// Markdown output every time.
//
// It deliberately still only looks at Judge Score, not the RAGMetrics
// added alongside it. Score is a single stable number a threshold can gate
// cleanly; RetrievalHit/MRR/citation correctness depend on knowledge base
// content and retrieval behavior that can legitimately shift for reasons
// that have nothing to do with a code regression (a document being
// re-uploaded, an embedding model change) — wiring those into the CI exit
// code now would risk failing builds on noise. They're surfaced in
// Compare's second table for a human to read, and can graduate into the
// gate later once there's enough run history to know what a real
// regression threshold for them should be.
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
