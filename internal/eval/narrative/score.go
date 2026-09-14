// Package narrative provides deterministic scoring for frozen narrative
// relation annotations. It deliberately has no model, database, or network
// dependency: a run can be recomputed from the same snapshots at any time.
package narrative

import (
	"sort"
	"strconv"
)

// Relation is the normalized exchange format for one asserted relation.
// Subject/Object must be stable entity IDs for end-to-end scoring. A separate
// conditional-identity run may supply human alias-mapped IDs, but its report
// must be labelled separately by its caller.
type Relation struct {
	Chapter     int      `json:"chapter"`
	Subject     string   `json:"subject"`
	Object      string   `json:"object"`
	Type        string   `json:"type"`
	Directed    bool     `json:"directed"`
	EvidenceIDs []string `json:"evidence_ids"`
}

// Result keeps counts alongside ratios so a small corpus cannot hide its
// sample size behind a percentage. Nil ratios mean N/A because their
// denominator is zero.
type Result struct {
	TruePositives     int      `json:"true_positives"`
	FalsePositives    int      `json:"false_positives"`
	FalseNegatives    int      `json:"false_negatives"`
	Precision         *float64 `json:"precision"`
	Recall            *float64 `json:"recall"`
	ValidEvidenceRate *float64 `json:"valid_evidence_rate"`
}

// Score matches at most one prediction to each truth unit. Both inputs are
// deduplicated by the evaluation unit (chapter, endpoints, type, direction),
// while evidence remains a set for citation-quality scoring.
func Score(truth, predicted []Relation) Result {
	truthByKey := deduplicate(truth)
	predictedByKey := deduplicate(predicted)
	result := Result{}

	for key := range predictedByKey {
		if _, ok := truthByKey[key]; ok {
			result.TruePositives++
		} else {
			result.FalsePositives++
		}
	}
	for key := range truthByKey {
		if _, ok := predictedByKey[key]; !ok {
			result.FalseNegatives++
		}
	}
	result.Precision = ratio(result.TruePositives, result.TruePositives+result.FalsePositives)
	result.Recall = ratio(result.TruePositives, result.TruePositives+result.FalseNegatives)

	valid, total := 0, 0
	for key, prediction := range predictedByKey {
		gold, matched := truthByKey[key]
		for _, evidenceID := range prediction.EvidenceIDs {
			total++
			if matched && contains(gold.EvidenceIDs, evidenceID) {
				valid++
			}
		}
	}
	result.ValidEvidenceRate = ratio(valid, total)
	return result
}

func deduplicate(relations []Relation) map[string]Relation {
	out := make(map[string]Relation, len(relations))
	for _, relation := range relations {
		key := relationKey(relation)
		current, exists := out[key]
		if !exists {
			current = relation
			current.EvidenceIDs = nil
		}
		for _, evidenceID := range relation.EvidenceIDs {
			if !contains(current.EvidenceIDs, evidenceID) {
				current.EvidenceIDs = append(current.EvidenceIDs, evidenceID)
			}
		}
		sort.Strings(current.EvidenceIDs)
		out[key] = current
	}
	return out
}

func relationKey(relation Relation) string {
	subject, object := relation.Subject, relation.Object
	if !relation.Directed && subject > object {
		subject, object = object, subject
	}
	return strconv.Itoa(relation.Chapter) + "\x00" + subject + "\x00" + object + "\x00" + relation.Type + "\x00" + strconv.FormatBool(relation.Directed)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func ratio(numerator, denominator int) *float64 {
	if denominator == 0 {
		return nil
	}
	value := float64(numerator) / float64(denominator)
	return &value
}
