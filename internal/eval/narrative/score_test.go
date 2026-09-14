package narrative

import "testing"

func TestScoreNormalizesUndirectedRelationsAndDeduplicatesPredictions(t *testing.T) {
	truth := []Relation{{Chapter: 2, Subject: "char-a", Object: "char-b", Type: "亲属", Directed: false, EvidenceIDs: []string{"C02P003"}}}
	predicted := []Relation{
		{Chapter: 2, Subject: "char-b", Object: "char-a", Type: "亲属", Directed: false, EvidenceIDs: []string{"C02P003", "C02P999"}},
		{Chapter: 2, Subject: "char-a", Object: "char-b", Type: "亲属", Directed: false, EvidenceIDs: []string{"C02P003"}},
	}

	got := Score(truth, predicted)
	if got.TruePositives != 1 || got.FalsePositives != 0 || got.FalseNegatives != 0 {
		t.Fatalf("counts = %+v, want TP=1 FP=0 FN=0", got)
	}
	if got.Precision == nil || *got.Precision != 1 || got.Recall == nil || *got.Recall != 1 {
		t.Fatalf("precision/recall = %+v, want 1/1", got)
	}
	if got.ValidEvidenceRate == nil || *got.ValidEvidenceRate != 0.5 {
		t.Fatalf("valid evidence rate = %v, want 0.5", got.ValidEvidenceRate)
	}
}

func TestScoreCountsMissingRelationAsFalseNegativeAndKeepsDirection(t *testing.T) {
	truth := []Relation{{Chapter: 1, Subject: "teacher", Object: "student", Type: "雇佣", Directed: true, EvidenceIDs: []string{"C01P001"}}}
	predicted := []Relation{{Chapter: 1, Subject: "student", Object: "teacher", Type: "雇佣", Directed: true, EvidenceIDs: []string{"C01P001"}}}

	got := Score(truth, predicted)
	if got.TruePositives != 0 || got.FalsePositives != 1 || got.FalseNegatives != 1 {
		t.Fatalf("directional mismatch counts = %+v, want TP=0 FP=1 FN=1", got)
	}
}

func TestScoreUsesNAForEmptyDenominators(t *testing.T) {
	got := Score(nil, nil)
	if got.Precision != nil || got.Recall != nil || got.ValidEvidenceRate != nil {
		t.Fatalf("empty score must be N/A, got %+v", got)
	}
}
