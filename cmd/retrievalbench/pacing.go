package main

import (
	"fmt"
	"time"

	"hify/internal/eval/retrievalbench"
)

// 014 FR-015/FR-016：benchmark-only 限速。只拉开相邻 query 的起点，检索、候选、
// Rerank 输入和 timeout 一概不变；只允许托管 API 的质量诊断使用。

// paceWait 返回下一条 query 开始前还需等待的时长。第一条 query（last 为零值）
// 和未限速（interval<=0）都不等待。
func paceWait(last, now time.Time, interval time.Duration) time.Duration {
	if interval <= 0 || last.IsZero() {
		return 0
	}
	if wait := interval - now.Sub(last); wait > 0 {
		return wait
	}
	return 0
}

func validateRerankPacing(source, mode string, interval time.Duration) error {
	if interval < 0 {
		return fmt.Errorf("pacing interval must not be negative")
	}
	if interval > 0 && (source != retrievalbench.RerankSourceHostedAPI || mode != retrievalbench.GateQuality) {
		return fmt.Errorf("pacing is only allowed for hosted_api quality_diagnostic runs")
	}
	return nil
}

func rerankPacingRecord(interval, totalWait time.Duration) *retrievalbench.RerankPacing {
	if interval <= 0 {
		return nil
	}
	return &retrievalbench.RerankPacing{
		IntervalMS:  interval.Milliseconds(),
		TotalWaitMS: totalWait.Milliseconds(),
		Reason:      retrievalbench.VoyageAccountRateLimitPacingReason,
	}
}
