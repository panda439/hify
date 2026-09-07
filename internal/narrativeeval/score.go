package narrativeeval

import (
	"sort"
	"strings"
)

// Relation is the model-independent scoring unit fixed by 010: one book
// version, chapter, normalized subject, relation type and normalized object.
type Relation struct {
	Subject  string     `json:"subject"`
	Object   string     `json:"object"`
	Type     string     `json:"type"`
	Chapter  int        `json:"chapter"`
	Evidence []Evidence `json:"evidence,omitempty"`
}

type Evidence struct {
	ParagraphID string `json:"paragraph_id,omitempty"`
	Quote       string `json:"quote"`
}

type Metrics struct {
	TP        int      `json:"tp"`
	FP        int      `json:"fp"`
	FN        int      `json:"fn"`
	Precision *float64 `json:"precision"`
	Recall    *float64 `json:"recall"`
}

type CitationMetrics struct {
	Valid int      `json:"valid"`
	Total int      `json:"total"`
	Rate  *float64 `json:"rate"`
}

type Report struct {
	EndToEnd         Metrics            `json:"end_to_end"`
	AliasConditional Metrics            `json:"alias_conditional"`
	ByType           map[string]Metrics `json:"by_type"`
	Citations        CitationMetrics    `json:"citations"`
}

var undirectedTypes = map[string]bool{
	"亲属": true, "同乡邻里": true, "冲突": true, "同伙": true,
}

type key struct {
	Chapter       int
	Subject, Type string
	Object        string
}

func relationKey(r Relation, aliases map[string]string) key {
	s, o := canonical(r.Subject, aliases), canonical(r.Object, aliases)
	if undirectedTypes[r.Type] && o < s {
		s, o = o, s
	}
	return key{Chapter: r.Chapter, Subject: s, Type: r.Type, Object: o}
}

func canonical(name string, aliases map[string]string) string {
	if v := aliases[name]; v != "" {
		return v
	}
	return name
}

// Score reports strict end-to-end identity quality and, separately, the
// relation score obtained when a truth alias table is allowed to normalize
// predicted names. Missing predictions remain false negatives, including
// relations located in failed extraction chunks.
func Score(truth, predicted []Relation, aliases map[string]string) Report {
	strictTruth := group(truth, nil)
	strictPred := group(predicted, nil)
	conditionalTruth := group(truth, aliases)
	conditionalPred := group(predicted, aliases)

	report := Report{
		EndToEnd:         compare(strictTruth, strictPred),
		AliasConditional: compare(conditionalTruth, conditionalPred),
		ByType:           byType(strictTruth, strictPred),
	}
	report.Citations = scoreCitations(strictTruth, strictPred)
	return report
}

func group(relations []Relation, aliases map[string]string) map[key][]Evidence {
	out := make(map[key][]Evidence)
	seenEvidence := make(map[key]map[string]bool)
	for _, r := range relations {
		k := relationKey(r, aliases)
		if _, ok := out[k]; !ok {
			out[k] = nil
			seenEvidence[k] = map[string]bool{}
		}
		for _, ev := range r.Evidence {
			id := ev.ParagraphID + "\x00" + ev.Quote
			if seenEvidence[k][id] {
				continue
			}
			seenEvidence[k][id] = true
			out[k] = append(out[k], ev)
		}
	}
	return out
}

func compare(truth, predicted map[key][]Evidence) Metrics {
	var m Metrics
	for k := range predicted {
		if _, ok := truth[k]; ok {
			m.TP++
		} else {
			m.FP++
		}
	}
	for k := range truth {
		if _, ok := predicted[k]; !ok {
			m.FN++
		}
	}
	if d := m.TP + m.FP; d > 0 {
		v := float64(m.TP) / float64(d)
		m.Precision = &v
	}
	if d := m.TP + m.FN; d > 0 {
		v := float64(m.TP) / float64(d)
		m.Recall = &v
	}
	return m
}

func byType(truth, predicted map[key][]Evidence) map[string]Metrics {
	types := map[string]bool{}
	for k := range truth {
		types[k.Type] = true
	}
	for k := range predicted {
		types[k.Type] = true
	}
	names := make([]string, 0, len(types))
	for typ := range types {
		names = append(names, typ)
	}
	sort.Strings(names)
	out := make(map[string]Metrics, len(names))
	for _, typ := range names {
		t, p := map[key][]Evidence{}, map[key][]Evidence{}
		for k, v := range truth {
			if k.Type == typ {
				t[k] = v
			}
		}
		for k, v := range predicted {
			if k.Type == typ {
				p[k] = v
			}
		}
		out[typ] = compare(t, p)
	}
	return out
}

func scoreCitations(truth, predicted map[key][]Evidence) CitationMetrics {
	var m CitationMetrics
	for k, predictedEvidence := range predicted {
		for _, ev := range predictedEvidence {
			m.Total++
			if citationSupported(ev, truth[k]) {
				m.Valid++
			}
		}
	}
	if m.Total > 0 {
		v := float64(m.Valid) / float64(m.Total)
		m.Rate = &v
	}
	return m
}

func citationSupported(predicted Evidence, supports []Evidence) bool {
	if predicted.Quote == "" {
		return false
	}
	for _, support := range supports {
		if predicted.ParagraphID != "" && support.ParagraphID != "" &&
			predicted.ParagraphID != support.ParagraphID {
			continue
		}
		// The annotation records a support range, often a whole paragraph;
		// model output is allowed to cite an exact shorter source substring.
		if strings.Contains(support.Quote, predicted.Quote) {
			return true
		}
	}
	return false
}
