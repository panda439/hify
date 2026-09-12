package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"hify/internal/eval/retrievalbench"
	"hify/internal/knowledge"
	"hify/internal/provider"
)

// 014：托管 Rerank（Voyage rerank-3）的 benchmark 入口。托管 API 没有本地
// sidecar，身份只能从 Hify 已配置的 Provider/Model 核对；这里只确认凭据存在，
// 从不读取或输出 API Key。

func benchmarkRerankSource(value string) (string, error) {
	switch value {
	case "", retrievalbench.RerankSourceLocalSidecar:
		return retrievalbench.RerankSourceLocalSidecar, nil
	case retrievalbench.RerankSourceHostedAPI:
		return retrievalbench.RerankSourceHostedAPI, nil
	}
	return "", fmt.Errorf("unsupported rerank source %q", value)
}

// precheckHostedRerank 固定"唯一实验变量"的身份：必须是启用中的 rerank-3
// 模型，挂在启用中的 Voyage 官方 API Provider 上，显式声明 rerank_format=voyage
// 且已保存凭据。任何一项不符都在发出付费请求前失败，不换模型、不降级。
func precheckHostedRerank(ctx context.Context, svc provider.Service, modelID string) (retrievalbench.RerankModelIdentity, error) {
	if modelID == "" {
		return retrievalbench.RerankModelIdentity{}, errors.New("hosted rerank precheck: HIFY_RAG_RERANK_MODEL_ID is required")
	}
	model, err := svc.GetModel(ctx, modelID)
	if err != nil {
		return retrievalbench.RerankModelIdentity{}, fmt.Errorf("hosted rerank precheck: %w", err)
	}
	if !model.IsActive || model.Capability != provider.CapabilityRerank || model.ModelName != retrievalbench.VoyageRerankModelName {
		return retrievalbench.RerankModelIdentity{}, fmt.Errorf("hosted rerank precheck: model must be an active %s rerank model", retrievalbench.VoyageRerankModelName)
	}
	p, err := svc.GetProvider(ctx, model.ProviderID)
	if err != nil {
		return retrievalbench.RerankModelIdentity{}, fmt.Errorf("hosted rerank precheck: %w", err)
	}
	if !p.IsActive || p.AdapterType != provider.AdapterOpenAICompatible || p.BaseURL != retrievalbench.VoyageAPIBaseURL ||
		p.AuthType != provider.AuthTypeAPIKey || !p.HasAPIKey || p.ExtraConfig.RerankFormat != provider.RerankFormatVoyage {
		return retrievalbench.RerankModelIdentity{}, errors.New("hosted rerank precheck: provider must be an active Voyage API provider with rerank_format=voyage and a stored credential")
	}
	return retrievalbench.RerankModelIdentity{
		Source:    retrievalbench.RerankSourceHostedAPI,
		ModelName: model.ModelName,
		Runtime: map[string]string{
			"provider_name": p.Name,
			"api_base_url":  p.BaseURL,
			"rerank_format": p.ExtraConfig.RerankFormat,
		},
		EndpointID: benchmarkServiceEndpointID(p.BaseURL),
		Ready:      true,
	}, nil
}

// hifyLatencyPercentiles 与 DiffRerankStats 使用同一个最近秩口径，
// 输入是 Hify 逐次 rerank 调用耗时（不是 query 总耗时，FR-007）。
func hifyLatencyPercentiles(durations []int64) (float64, float64) {
	if len(durations) == 0 {
		return 0, 0
	}
	sorted := append([]int64(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	at := func(p float64) float64 {
		idx := int(p * float64(len(sorted)))
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return float64(sorted[idx])
	}
	return at(0.5), at(0.95)
}

// successfulRerankDurations is the only source for hosted p50/p95 samples.
// A request that degraded (including timeout, 429, circuit-open, or invalid
// response) is not a successful applied call and must not enter the sample.
func successfulRerankDurations(results []knowledge.BenchmarkRetrievalResult) []int64 {
	durations := make([]int64, 0, len(results))
	for _, result := range results {
		if result.RerankApplied && result.RerankInputCount > 0 {
			durations = append(durations, result.RerankDurationMS)
		}
	}
	return durations
}

func aggregateHostedFailureKinds(results []knowledge.BenchmarkRetrievalResult) map[string]int {
	counts := map[string]int{
		string(provider.RerankFailureTimeout):         0,
		string(provider.RerankFailureHTTP429):         0,
		string(provider.RerankFailureCircuitOpen):     0,
		string(provider.RerankFailureResponseInvalid): 0,
		string(provider.RerankFailureOther):           0,
	}
	for _, result := range results {
		if result.RerankFailureKind == "" {
			continue
		}
		if _, ok := counts[result.RerankFailureKind]; !ok {
			counts[string(provider.RerankFailureOther)]++
			continue
		}
		counts[result.RerankFailureKind]++
	}
	return counts
}

func runQueryErrorMarker(hosted bool, err error) string {
	if err == nil {
		return ""
	}
	if hosted {
		return "query_failed"
	}
	return err.Error()
}

// hostedRerankPhaseStats 只填 Hify observer 实际观测到的字段，sidecar 计数
// 保持为零——托管 API 不存在 sidecar，不能凭 HTTP 成功推断 Hify 已应用。
func hostedRerankPhaseStats(queryCount, enabled, applied, degraded, inputCount int, durationMS int64, durations []int64, totalTokens int) *retrievalbench.RerankPhaseStats {
	p50, p95 := hifyLatencyPercentiles(durations)
	stats := &retrievalbench.RerankPhaseStats{
		HifyEnabledCount:  enabled,
		HifyAppliedCount:  applied,
		HifyDegradedCount: degraded,
		HifyInputCount:    inputCount,
		HifyDurationMS:    durationMS,
		HifyP50MS:         p50,
		HifyP95MS:         p95,
		HifyLatencyStatus: "unavailable",
		HifyTotalTokens:   totalTokens,
		HifyOutcome:       "not_all_applied_or_degraded",
	}
	if len(durations) > 0 {
		stats.HifyLatencyStatus = "available"
	}
	if enabled == queryCount && applied == queryCount && degraded == 0 {
		stats.HifyOutcome = "all_applied"
	}
	return stats
}

// validateGateRerankTimeout 让指纹里的 timeout 与实际生效值一致：部署门禁
// 固定 1.5s，质量诊断固定 30s，配置漂移时直接拒绝运行。
func validateGateRerankTimeout(mode string, actual time.Duration) error {
	var expected time.Duration
	switch mode {
	case retrievalbench.GateDeployment:
		expected = 1500 * time.Millisecond
	case retrievalbench.GateQuality:
		expected = 30 * time.Second
	default:
		return fmt.Errorf("unsupported benchmark run mode %q", mode)
	}
	if actual != expected {
		return fmt.Errorf("%s requires rerank timeout %s, got %s", mode, expected, actual)
	}
	return nil
}

// ensureFreshOutput：付费运行的 raw run 不覆盖、不自动重跑（FR-013）。
func ensureFreshOutput(path string) error {
	_, err := os.Stat(path)
	if err == nil {
		return fmt.Errorf("output %s already exists; paid hosted runs are never overwritten or repeated automatically", path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func applyRerankFingerprint(fp *retrievalbench.Fingerprint, modelName, modelDigest, mode string, timeout time.Duration) {
	fp.RerankEnabled = true
	fp.RerankModelName = modelName
	fp.RerankModelDigest = modelDigest
	fp.RerankCandidateLimit = 50
	fp.RerankTimeoutMS = timeout.Milliseconds()
	fp.RunMode = mode
}

// gateDecision 先用单变量 compare 证明只有 Rerank 不同，再按 014 的模型无关
// 标准给出部署或质量结论；两种门禁的输出文件由调用方分开命名。
func gateDecision(c commandConfig) error {
	if c.gate == "" || c.baseline == "" || c.candidate == "" || c.output == "" {
		return errors.New("gate-decision requires --gate, --baseline, --candidate and --output")
	}
	if c.gate != retrievalbench.GateDeployment && c.gate != retrievalbench.GateQuality {
		return fmt.Errorf("unsupported gate %q", c.gate)
	}
	var baseline, candidate retrievalbench.MetricReport
	if err := readJSON(c.baseline, &baseline); err != nil {
		return err
	}
	if err := readJSON(c.candidate, &candidate); err != nil {
		return err
	}
	var comparison retrievalbench.ComparisonReport
	var err error
	if c.gate == retrievalbench.GateQuality {
		comparison, err = retrievalbench.CompareQualityExperiment(baseline, candidate)
	} else {
		comparison, err = retrievalbench.CompareRerankExperiment(baseline, candidate)
	}
	if err != nil {
		return err
	}
	return retrievalbench.SaveJSON(c.output, rerankDecisionReport{
		Comparison: comparison,
		Decision: retrievalbench.DecideRerankGate(retrievalbench.RerankGateInput{
			Gate: c.gate, Baseline: baseline, Candidate: candidate, Comparable: !comparison.NonComparable,
		}),
	})
}
