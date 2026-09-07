package narrativeeval

import "testing"

func TestValidateReferenceFindsMissingParagraphAndMismatchedQuote(t *testing.T) {
	paragraphs := map[string]string{"C01P001": "甲打了乙。"}
	relations := []Relation{
		{Subject: "甲", Object: "乙", Type: "欺凌", Chapter: 1, Evidence: []Evidence{{ParagraphID: "C01P001", Quote: "甲打了乙"}}},
		{Subject: "甲", Object: "丙", Type: "冲突", Chapter: 1, Evidence: []Evidence{{ParagraphID: "C01P404", Quote: "不存在"}}},
		{Subject: "乙", Object: "丙", Type: "冲突", Chapter: 1, Evidence: []Evidence{{ParagraphID: "C01P001", Quote: "乙骂了丙"}}},
	}

	issues := ValidateReference(relations, paragraphs)
	if len(issues) != 2 || issues[0].Kind != "missing_paragraph" || issues[1].Kind != "quote_mismatch" {
		t.Fatalf("issues = %+v", issues)
	}
}

func TestParseParagraphCorpusRejectsDuplicateIDs(t *testing.T) {
	_, err := ParseParagraphCorpus([]byte("[C01P001] 第一段\n\n[C01P001] 重复段\n"))
	if err == nil {
		t.Fatal("expected duplicate paragraph id to fail")
	}
}
