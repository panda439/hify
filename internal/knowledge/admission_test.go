package knowledge

import "testing"

// --- Task 1, scenarios 6-9: rrfFuse orchestration-level admission tests ---
// These exercise admitBySourceSignal wired into rrfFuse (hybrid.go),
// including its interaction with topK truncation and Phase 5 content-dedup
// — still zero database dependency, same as every other hybrid_test.go
// case.

// 6. A(拒绝)、B(通过)、C(通过) + topK=2 返回 B、C — a rejected candidate
// must not consume a topK slot, and must not block admitted candidates
// ranked below it from appearing in the result.
func TestRRFFuseAdmissionRejectedCandidateNeverConsumesTopKSlot(t *testing.T) {
	// Rank order (vector list position = RRF rank) is A, B, C — A ranks
	// highest but its score (0.2) is below vectorAdmissionThreshold (0.35)
	// with no keyword signal, so it must be rejected outright.
	vector := []RetrievedChunk{rc("a-rejected", 0.2), rc("b", 0.9), rc("c", 0.5)}

	got, stats := fuseTopK(vector, nil, 2)

	want := []string{"b", "c"}
	if got := idsOf(got); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v — a-rejected must be dropped before topK so b and c both survive", got, want)
	}
	if stats.AdmissionRejectedCount != 1 {
		t.Fatalf("AdmissionRejectedCount = %d, want 1", stats.AdmissionRejectedCount)
	}
}

// 7. A、B 正文相同：A 排名更高但不达标，B 排名稍低但达标，最终必须保留 B
// — admission must run BEFORE content-dedup so the unqualified higher-rank
// duplicate is gone before dedup ever has to choose between the two.
func TestRRFFuseAdmissionBeforeDedupKeepsAdmittedDuplicateOverRejectedHigherRank(t *testing.T) {
	vector := []RetrievedChunk{
		rcContent("a-rejected", 0.2, "重复正文"), // rank 1, below threshold
		rcContent("b-admitted", 0.5, "重复正文"), // rank 2, admitted
	}

	got, _ := fuseTopK(vector, nil, 10)

	if len(got) != 1 || got[0].ID != "b-admitted" {
		t.Fatalf("got %v, want exactly [b-admitted] — the higher-ranked but unqualified duplicate must be rejected first, leaving the admitted one to survive dedup", idsOf(got))
	}
}

// 8. A、B 正文相同且都达标，仍保留 RRF 排名更高的 A — once both survive
// admission, ordinary Phase 5 dedup precedence (highest fusionScore first)
// applies unchanged.
func TestRRFFuseAdmissionThenDedupKeepsHigherRankedWhenBothAdmitted(t *testing.T) {
	vector := []RetrievedChunk{
		rcContent("a-higher-rank", 0.9, "重复正文"), // rank 1, admitted
		rcContent("b-lower-rank", 0.5, "重复正文"),  // rank 2, admitted
	}

	got, _ := fuseTopK(vector, nil, 10)

	if len(got) != 1 || got[0].ID != "a-higher-rank" {
		t.Fatalf("got %v, want exactly [a-higher-rank] — both admitted, so ordinary dedup precedence (higher fusion rank wins) applies", idsOf(got))
	}
}

// 9. 准入和内容去重均不得改写保留项的 Score/Citation 元数据.
func TestRRFFuseAdmissionAndDedupPreserveSurvivorFields(t *testing.T) {
	docName := "policy.pdf"
	page := 3
	section := "1.1 Scope"
	survivor := RetrievedChunk{
		Chunk: Chunk{
			ID:           "survivor",
			Content:      "重复正文",
			DocumentName: docName,
			PageNumber:   &page,
			PageEnd:      &page,
			SectionTitle: &section,
		},
		Score: 0.5, // admitted (>= vectorAdmissionThreshold)
	}
	rejectedDuplicate := RetrievedChunk{
		Chunk: Chunk{ID: "rejected-dup", Content: "重复正文"},
		Score: 0.1, // rejected (< vectorAdmissionThreshold, no keyword signal)
	}

	// rejectedDuplicate ranked first (higher RRF rank) so this also proves
	// admission — not dedup rank order — is what determines the survivor.
	vector := []RetrievedChunk{rejectedDuplicate, survivor}

	got, _ := fuseTopK(vector, nil, 10)

	if len(got) != 1 || got[0].ID != "survivor" {
		t.Fatalf("got %v, want exactly [survivor]", idsOf(got))
	}
	kept := got[0]
	if kept.Score != 0.5 {
		t.Fatalf("Score = %f, want unchanged 0.5", kept.Score)
	}
	if kept.DocumentName != docName {
		t.Fatalf("DocumentName = %q, want unchanged %q", kept.DocumentName, docName)
	}
	if kept.PageNumber == nil || *kept.PageNumber != page {
		t.Fatalf("PageNumber = %v, want unchanged %d", kept.PageNumber, page)
	}
	if kept.SectionTitle == nil || *kept.SectionTitle != section {
		t.Fatalf("SectionTitle = %v, want unchanged %q", kept.SectionTitle, section)
	}
}

// Task 1, scenarios 1-5 of the Phase 8 plan: admitBySourceSignal's pure
// per-path threshold logic, with zero rrfFuse/database involvement.

// 1. vector 分数 < 0.35 拒绝，== 0.35 和 > 0.35 通过.
func TestAdmitBySourceSignalVectorThreshold(t *testing.T) {
	cases := []struct {
		name  string
		score float64
		want  bool
	}{
		{"below", 0.34, false},
		{"equal", 0.35, true},
		{"above", 0.36, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := admitBySourceSignal(true, tc.score, false, 0)
			if got.admitted != tc.want {
				t.Fatalf("admitBySourceSignal(vector=%v) admitted=%v, want %v", tc.score, got.admitted, tc.want)
			}
		})
	}
}

// 2. keyword 分数 < 0.45 拒绝，== 0.45 和 > 0.45 通过.
func TestAdmitBySourceSignalKeywordThreshold(t *testing.T) {
	cases := []struct {
		name  string
		score float64
		want  bool
	}{
		{"below", 0.44, false},
		{"equal", 0.45, true},
		{"above", 0.46, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := admitBySourceSignal(false, 0, true, tc.score)
			if got.admitted != tc.want {
				t.Fatalf("admitBySourceSignal(keyword=%v) admitted=%v, want %v", tc.score, got.admitted, tc.want)
			}
		})
	}
}

// 3. 双路命中时任意一路达标即通过.
func TestAdmitBySourceSignalEitherPathAdmits(t *testing.T) {
	// vector weak, keyword strong.
	if got := admitBySourceSignal(true, 0.1, true, 0.9); !got.admitted {
		t.Fatalf("weak vector + strong keyword: admitted=false, want true")
	}
	// vector strong, keyword weak.
	if got := admitBySourceSignal(true, 0.9, true, 0.1); !got.admitted {
		t.Fatalf("strong vector + weak keyword: admitted=false, want true")
	}
	// both strong.
	if got := admitBySourceSignal(true, 0.9, true, 0.9); !got.admitted {
		t.Fatalf("both strong: admitted=false, want true")
	}
}

// 4. 两路都未达标拒绝.
func TestAdmitBySourceSignalBothBelowRejects(t *testing.T) {
	got := admitBySourceSignal(true, 0.2, true, 0.3)
	if got.admitted {
		t.Fatalf("both below threshold: admitted=true, want false")
	}
	if !got.belowVectorSignal || !got.belowKeywordSignal {
		t.Fatalf("belowVectorSignal=%v belowKeywordSignal=%v, want both true", got.belowVectorSignal, got.belowKeywordSignal)
	}
}

// 5. 缺少某一路信号时不能把零值当作真实分数：haveVector=false 时
// vectorScore 的零值绝不能被当成"vector 分数为 0，未达标"参与判断，也不能
// 被计入 belowVectorSignal。
func TestAdmitBySourceSignalMissingPathIsNotAZeroScore(t *testing.T) {
	// Only keyword signal exists and passes; vector was never hit at all.
	got := admitBySourceSignal(false, 0, true, 0.9)
	if !got.admitted {
		t.Fatalf("keyword-only strong hit: admitted=false, want true")
	}
	if got.belowVectorSignal {
		t.Fatalf("belowVectorSignal=true for a candidate with NO vector signal at all — a missing signal must never be reported as a failing one")
	}

	// Only vector signal exists and passes; keyword was never hit at all.
	got2 := admitBySourceSignal(true, 0.9, false, 0)
	if !got2.admitted {
		t.Fatalf("vector-only strong hit: admitted=false, want true")
	}
	if got2.belowKeywordSignal {
		t.Fatalf("belowKeywordSignal=true for a candidate with NO keyword signal at all — a missing signal must never be reported as a failing one")
	}

	// Neither path present at all: must not be silently admitted by
	// comparing two zero-values that both happen to fail their thresholds
	// "for the right reason" — it must fail because there is genuinely no
	// evidence, and belowVectorSignal/belowKeywordSignal must both stay
	// false (nothing to report as "below" when nothing was ever measured).
	got3 := admitBySourceSignal(false, 0, false, 0)
	if got3.admitted {
		t.Fatalf("no signal at all: admitted=true, want false")
	}
	if got3.belowVectorSignal || got3.belowKeywordSignal {
		t.Fatalf("belowVectorSignal=%v belowKeywordSignal=%v, want both false when neither path was ever hit", got3.belowVectorSignal, got3.belowKeywordSignal)
	}
}
