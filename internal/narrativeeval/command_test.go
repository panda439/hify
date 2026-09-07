package narrativeeval

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRunScoreIsDeterministicAndRecordsInputHashes(t *testing.T) {
	dir := t.TempDir()
	truth := filepath.Join(dir, "truth.jsonl")
	pred := filepath.Join(dir, "prediction.json")
	aliases := filepath.Join(dir, "aliases.json")
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(truth, `{"subject":"阿Q","object":"赵太爷","type":"冲突","chapter":1}`+"\n")
	write(pred, `{"schema_version":1,"reference_kind":"ai_draft","model":{"name":"qwen","digest":"sha256:x"},"relations":[{"subject":"阿Q","object":"赵太爷","type":"冲突","chapter":1}]}`)
	write(aliases, `[]`)

	var first, second bytes.Buffer
	if err := RunScore(truth, pred, aliases, &first); err != nil {
		t.Fatal(err)
	}
	if err := RunScore(truth, pred, aliases, &second); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatalf("same snapshot produced different reports\nfirst=%s\nsecond=%s", first.String(), second.String())
	}
	if !bytes.Contains(first.Bytes(), []byte(`"truth_sha256"`)) || !bytes.Contains(first.Bytes(), []byte(`"tp": 1`)) {
		t.Fatalf("report lacks hashes or score: %s", first.String())
	}
}
