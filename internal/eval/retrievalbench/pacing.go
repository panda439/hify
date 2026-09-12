package retrievalbench

import "fmt"

// VoyageAccountRateLimitPacingReason 是 014 FR-015 固定的限速原因：账户未添加
// 付款方式时 Voyage 限为 3 RPM / 10K TPM，只能靠拉开 query 间隔拿到完整质量证据。
const VoyageAccountRateLimitPacingReason = "benchmark-only pacing for a Voyage account without a payment method (3 RPM / 10K TPM); retrieval, rerank inputs, timeout and decision thresholds are unchanged"

// RerankPacing 只记录"相邻 query 起点至少间隔多久、总共等了多久、为什么限速"。
// 它不进入指纹：限速不改变检索、候选、Rerank 输入或 timeout，但会让 query 阶段
// 总耗时包含等待，读报告时要以 Hify 逐次 rerank 耗时（HifyP50MS/HifyP95MS）为准。
type RerankPacing struct {
	IntervalMS  int64  `json:"interval_ms"`
	TotalWaitMS int64  `json:"total_wait_ms"`
	Reason      string `json:"reason"`
}

func (p RerankPacing) validate() error {
	if p.IntervalMS <= 0 || p.TotalWaitMS < 0 {
		return fmt.Errorf("rerank pacing is invalid")
	}
	if p.Reason == "" {
		return fmt.Errorf("rerank pacing reason is missing")
	}
	return nil
}
