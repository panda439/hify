package main

import (
	"testing"
	"time"

	"hify/internal/eval/retrievalbench"
)

// 014 FR-015/FR-016：benchmark-only 限速让相邻 query 起点至少间隔固定时长，
// 只允许托管 API 的质量诊断使用；间隔之外的检索、候选、timeout 一概不变。

func TestPaceWaitKeepsQueryStartsApart(t *testing.T) {
	start := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		last, now time.Time
		interval  time.Duration
		want      time.Duration
	}{
		"first query never waits":          {time.Time{}, start, 65 * time.Second, 0},
		"waits for the remaining interval": {start, start.Add(10 * time.Second), 65 * time.Second, 55 * time.Second},
		"no wait once interval elapsed":    {start, start.Add(70 * time.Second), 65 * time.Second, 0},
		"pacing disabled":                  {start, start.Add(10 * time.Second), 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			if got := paceWait(tc.last, tc.now, tc.interval); got != tc.want {
				t.Fatalf("paceWait = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestValidateRerankPacingOnlyForHostedQuality(t *testing.T) {
	hosted, local := retrievalbench.RerankSourceHostedAPI, retrievalbench.RerankSourceLocalSidecar
	for _, ok := range []struct {
		source, mode string
		interval     time.Duration
	}{
		{hosted, "quality_diagnostic", 65 * time.Second},
		{hosted, "quality_diagnostic", 0},
		{hosted, "deployment_gate", 0},
		{local, "deployment_gate", 0},
	} {
		if err := validateRerankPacing(ok.source, ok.mode, ok.interval); err != nil {
			t.Fatalf("%s/%s/%s rejected: %v", ok.source, ok.mode, ok.interval, err)
		}
	}
	for _, bad := range []struct {
		source, mode string
		interval     time.Duration
	}{
		{hosted, "deployment_gate", 65 * time.Second},
		{local, "quality_diagnostic", 65 * time.Second},
		{hosted, "quality_diagnostic", -time.Second},
	} {
		if err := validateRerankPacing(bad.source, bad.mode, bad.interval); err == nil {
			t.Fatalf("%s/%s/%s must be rejected", bad.source, bad.mode, bad.interval)
		}
	}
}

func TestRerankPacingRecordOnlyWhenPaced(t *testing.T) {
	if got := rerankPacingRecord(0, 0); got != nil {
		t.Fatalf("unpaced run must not carry pacing evidence: %+v", got)
	}
	got := rerankPacingRecord(65*time.Second, 3185*time.Second)
	if got == nil || got.IntervalMS != 65000 || got.TotalWaitMS != 3185000 || got.Reason == "" {
		t.Fatalf("unexpected pacing record: %+v", got)
	}
}

func TestParseRunAcceptsPacingInterval(t *testing.T) {
	cfg, err := parseArgs([]string{"retrievalbench", "run", "--rerank-source", "hosted_api", "--run-mode", "quality_diagnostic", "--pacing-interval", "65s", "--input", "dataset", "--output", "run.json"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.pacingInterval != 65*time.Second {
		t.Fatalf("pacing interval = %s, want 65s", cfg.pacingInterval)
	}
	cfg, err = parseArgs([]string{"retrievalbench", "run", "--input", "dataset", "--output", "run.json"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.pacingInterval != 0 {
		t.Fatalf("default pacing interval = %s, want 0", cfg.pacingInterval)
	}
}
