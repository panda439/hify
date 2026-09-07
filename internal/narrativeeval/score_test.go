package narrativeeval

import "testing"

func TestScoreNormalizesUndirectedRelationsAndDeduplicatesPredictions(t *testing.T) {
	truth := []Relation{
		{Subject: "甲", Object: "乙", Type: "亲属", Chapter: 1, Evidence: []Evidence{{Quote: "甲乙是兄弟"}}},
		{Subject: "甲", Object: "丙", Type: "欺凌", Chapter: 2, Evidence: []Evidence{{Quote: "甲打了丙"}}},
	}
	predicted := []Relation{
		{Subject: "乙", Object: "甲", Type: "亲属", Chapter: 1, Evidence: []Evidence{{Quote: "甲乙是兄弟"}, {Quote: "错误引用"}}},
		{Subject: "乙", Object: "甲", Type: "亲属", Chapter: 1, Evidence: []Evidence{{Quote: "甲乙是兄弟"}}},
		{Subject: "甲", Object: "丙", Type: "欺凌", Chapter: 3, Evidence: []Evidence{{Quote: "甲打了丙"}}},
	}

	report := Score(truth, predicted, nil)
	if report.EndToEnd.TP != 1 || report.EndToEnd.FP != 1 || report.EndToEnd.FN != 1 {
		t.Fatalf("counts = %+v, want TP=1 FP=1 FN=1", report.EndToEnd)
	}
	if report.EndToEnd.Precision == nil || *report.EndToEnd.Precision != 0.5 {
		t.Fatalf("precision = %v, want 0.5", report.EndToEnd.Precision)
	}
	if report.EndToEnd.Recall == nil || *report.EndToEnd.Recall != 0.5 {
		t.Fatalf("recall = %v, want 0.5", report.EndToEnd.Recall)
	}
	if report.Citations.Valid != 1 || report.Citations.Total != 3 {
		t.Fatalf("citations = %+v, want valid=1 total=3", report.Citations)
	}
}

func TestScoreKeepsDirectedRelationsDirected(t *testing.T) {
	truth := []Relation{{Subject: "甲", Object: "乙", Type: "欺凌", Chapter: 1}}
	predicted := []Relation{{Subject: "乙", Object: "甲", Type: "欺凌", Chapter: 1}}

	report := Score(truth, predicted, nil)
	if report.EndToEnd.TP != 0 || report.EndToEnd.FP != 1 || report.EndToEnd.FN != 1 {
		t.Fatalf("counts = %+v, want reversed directed relation to miss", report.EndToEnd)
	}
}

func TestScoreReportsAliasConditionalSeparately(t *testing.T) {
	truth := []Relation{{Subject: "阿Q", Object: "赵太爷", Type: "冲突", Chapter: 1}}
	predicted := []Relation{{Subject: "老Q", Object: "赵太爷", Type: "冲突", Chapter: 1}}
	aliases := map[string]string{"老Q": "阿Q"}

	report := Score(truth, predicted, aliases)
	if report.EndToEnd.TP != 0 {
		t.Fatalf("end-to-end TP = %d, want 0", report.EndToEnd.TP)
	}
	if report.AliasConditional.TP != 1 {
		t.Fatalf("alias-conditional TP = %d, want 1", report.AliasConditional.TP)
	}
}

func TestScoreUsesNAForEmptyDenominators(t *testing.T) {
	report := Score(nil, nil, nil)
	if report.EndToEnd.Precision != nil || report.EndToEnd.Recall != nil || report.Citations.Rate != nil {
		t.Fatalf("empty denominators must be N/A: %+v", report)
	}
}

func TestScoreAcceptsExactSourceQuoteInsideAnnotatedSupportRange(t *testing.T) {
	truth := []Relation{{
		Subject: "甲", Object: "乙", Type: "冲突", Chapter: 1,
		Evidence: []Evidence{{ParagraphID: "C01P001", Quote: "甲先责骂乙，随后打了乙一个耳光。"}},
	}}
	predicted := []Relation{{
		Subject: "甲", Object: "乙", Type: "冲突", Chapter: 1,
		Evidence: []Evidence{{Quote: "打了乙一个耳光"}},
	}}

	report := Score(truth, predicted, nil)
	if report.Citations.Valid != 1 || report.Citations.Total != 1 {
		t.Fatalf("citations = %+v, want source quote inside support range to be valid", report.Citations)
	}
}
