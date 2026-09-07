package narrativeeval

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

var paragraphStart = regexp.MustCompile(`^\[([A-Z][A-Z0-9]+)\]\s?(.*)$`)

type ReferenceIssue struct {
	RelationIndex int    `json:"relation_index"`
	EvidenceIndex int    `json:"evidence_index"`
	ParagraphID   string `json:"paragraph_id"`
	Kind          string `json:"kind"`
}

func ParseParagraphCorpus(body []byte) (map[string]string, error) {
	out := map[string]string{}
	var currentID string
	var current strings.Builder
	flush := func() error {
		if currentID == "" {
			return nil
		}
		if _, exists := out[currentID]; exists {
			return fmt.Errorf("duplicate paragraph id %s", currentID)
		}
		out[currentID] = strings.TrimSpace(current.String())
		current.Reset()
		return nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if match := paragraphStart.FindStringSubmatch(line); match != nil {
			if err := flush(); err != nil {
				return nil, err
			}
			currentID = match[1]
			current.WriteString(match[2])
			continue
		}
		if currentID != "" && strings.TrimSpace(line) != "" {
			current.WriteByte('\n')
			current.WriteString(line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

func ValidateReference(relations []Relation, paragraphs map[string]string) []ReferenceIssue {
	var issues []ReferenceIssue
	for i, relation := range relations {
		for j, evidence := range relation.Evidence {
			paragraph, ok := paragraphs[evidence.ParagraphID]
			if !ok {
				issues = append(issues, ReferenceIssue{i, j, evidence.ParagraphID, "missing_paragraph"})
				continue
			}
			if evidence.Quote == "" || !strings.Contains(paragraph, evidence.Quote) {
				issues = append(issues, ReferenceIssue{i, j, evidence.ParagraphID, "quote_mismatch"})
			}
		}
	}
	return issues
}
