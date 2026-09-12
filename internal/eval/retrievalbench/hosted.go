package retrievalbench

import (
	"fmt"
	"math"
)

// 014：托管 Rerank（Voyage rerank-3）没有本地 /health、/stats sidecar。
// 身份来自 Hify 数据库里的 Provider/Model 配置（由 CLI precheck 固定），
// 真实性来自 Hify observer 的 applied/degraded/input/duration，以及 Voyage
// 成功响应的 usage.total_tokens。这里不保存 API Key、query、正文或逐条请求。
const (
	RerankSourceLocalSidecar = "local_sidecar"
	RerankSourceHostedAPI    = "hosted_api"

	VoyageRerankModelName = "rerank-3"
	VoyageAPIBaseURL      = "https://api.voyageai.com/v1"

	// 价格快照取评测当日官方定价页，真实结果产生后不得修改（FR-008/FR-009）。
	VoyagePriceSourceURL              = "https://docs.voyageai.com/docs/pricing"
	VoyagePriceSnapshotDate           = "2026-09-11"
	VoyageRerank3PricePerMillionUSD   = 0.05
	VoyageRerank3FreeTokensPerAccount = int64(200_000_000)

	// FR-013：单次实验每个 query 最多送 50 条候选。
	hostedRerankCandidateLimit = 50
)

// HostedRerankCost 把"列表价估算"和"免费额度"分开表述：API 响应不暴露账户
// 剩余免费额度，所以列表价不能写成实际扣款。
type HostedRerankCost struct {
	ModelName            string  `json:"model_name"`
	APIBaseURL           string  `json:"api_base_url"`
	EvaluatedAt          string  `json:"evaluated_at"`
	TotalTokens          int     `json:"total_tokens"`
	TokenCountBasis      string  `json:"token_count_basis"`
	PricePerMillionUSD   float64 `json:"price_per_million_usd"`
	ListPriceUSD         float64 `json:"list_price_usd"`
	PriceSourceURL       string  `json:"price_source_url"`
	PriceSnapshotDate    string  `json:"price_snapshot_date"`
	FreeTokensPerAccount int64   `json:"free_tokens_per_account"`
	BillingNote          string  `json:"billing_note"`
}

// EstimateListPriceUSD 是纯函数：tokens / 1M × 单价。负 token、非有限或负单价
// 一律拒绝，避免把坏输入算成看似合理的金额。
func EstimateListPriceUSD(totalTokens int, pricePerMillionUSD float64) (float64, error) {
	if totalTokens < 0 {
		return 0, fmt.Errorf("total tokens must not be negative")
	}
	if math.IsNaN(pricePerMillionUSD) || math.IsInf(pricePerMillionUSD, 0) || pricePerMillionUSD < 0 {
		return 0, fmt.Errorf("price per million tokens must be a finite non-negative number")
	}
	return float64(totalTokens) / 1_000_000 * pricePerMillionUSD, nil
}

// NewVoyageRerankCost 用固定价格快照生成费用证据。
func NewVoyageRerankCost(totalTokens int, evaluatedAt string) (HostedRerankCost, error) {
	if evaluatedAt == "" {
		return HostedRerankCost{}, fmt.Errorf("evaluation time is required")
	}
	price, err := EstimateListPriceUSD(totalTokens, VoyageRerank3PricePerMillionUSD)
	if err != nil {
		return HostedRerankCost{}, err
	}
	return HostedRerankCost{
		ModelName:            VoyageRerankModelName,
		APIBaseURL:           VoyageAPIBaseURL,
		EvaluatedAt:          evaluatedAt,
		TotalTokens:          totalTokens,
		TokenCountBasis:      "sum of usage.total_tokens from Voyage responses received by Hify; calls that timed out on the Hify side may still be billed and are not counted",
		PricePerMillionUSD:   VoyageRerank3PricePerMillionUSD,
		ListPriceUSD:         price,
		PriceSourceURL:       VoyagePriceSourceURL,
		PriceSnapshotDate:    VoyagePriceSnapshotDate,
		FreeTokensPerAccount: VoyageRerank3FreeTokensPerAccount,
		BillingNote:          "list price estimate only; the first 200M rerank-3 tokens per account are free, and the remaining allowance is not exposed by the API response",
	}, nil
}

// validateHostedRerankEvidence 是托管 API 的证据规则：必须是固定模型、
// 50 次全部 applied 且零降级、有输入数和真实 token 用量、延迟有限且有序，
// 并且不得出现任何 sidecar 计数（托管 API 不存在 sidecar，出现即说明制品混用）。
func validateHostedRerankEvidence(report MetricReport) error {
	identity := report.RerankIdentity
	if identity.ModelName != VoyageRerankModelName || identity.EndpointID == "" {
		return fmt.Errorf("hosted rerank identity does not match fixed model")
	}
	s := report.RerankStats
	if s == nil {
		return fmt.Errorf("rerank phase stats are missing")
	}
	if s.RequestCount != 0 || s.SuccessCount != 0 || s.FailureCount != 0 || s.DegradedCount != 0 ||
		s.CandidateCountTotal != 0 || s.StatsBefore != nil || s.StatsAfter != nil {
		return fmt.Errorf("hosted rerank evidence must not carry sidecar counters")
	}
	if s.HifyEnabledCount < 0 || s.HifyAppliedCount < 0 || s.HifyDegradedCount < 0 ||
		s.HifyAppliedCount > s.HifyEnabledCount || s.HifyDegradedCount > s.HifyEnabledCount {
		return fmt.Errorf("hify rerank counters are invalid")
	}
	if report.QueryCount != s.HifyEnabledCount || s.HifyAppliedCount != report.QueryCount ||
		s.HifyDegradedCount != 0 || s.HifyOutcome != "all_applied" {
		return fmt.Errorf("hify rerank outcome is incomplete")
	}
	if s.HifyInputCount <= 0 {
		return fmt.Errorf("hify rerank input count is missing")
	}
	if s.HifyInputCount > report.QueryCount*hostedRerankCandidateLimit {
		return fmt.Errorf("hify rerank input count exceeds the per-query candidate limit")
	}
	if s.HifyTotalTokens <= 0 {
		return fmt.Errorf("hosted rerank token usage is missing")
	}
	if s.HifyFailureCounts == nil {
		return fmt.Errorf("hosted rerank failure counts are missing")
	}
	fixedKinds := map[string]struct{}{
		"timeout": {}, "http_429": {}, "circuit_open": {}, "response_invalid": {}, "other": {},
	}
	failureTotal := 0
	for kind, count := range s.HifyFailureCounts {
		if _, ok := fixedKinds[kind]; !ok {
			return fmt.Errorf("hosted rerank failure kind %q is not allowed", kind)
		}
		if count < 0 {
			return fmt.Errorf("hosted rerank failure count for %q is negative", kind)
		}
		failureTotal += count
	}
	if failureTotal != s.HifyDegradedCount {
		return fmt.Errorf("hosted rerank failure counts do not match degraded count")
	}
	if s.HifyLatencyStatus != "available" {
		return fmt.Errorf("hosted rerank success latency is unavailable")
	}
	if !finiteNonNegative(s.HifyP50MS) || !finiteNonNegative(s.HifyP95MS) || s.HifyP95MS < s.HifyP50MS {
		return fmt.Errorf("hify rerank latency is not finite or ordered")
	}
	return nil
}

func finiteNonNegative(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0
}
