package narrativeeval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRelationsJSONLReadsAnnotationShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relations.jsonl")
	body := `{"id":"R1","subject":"阿Q","object":"赵太爷","type":"冲突","chapter":1,"evidence":[{"paragraph_id":"C01P007","quote":"赵太爷打阿Q"}]}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadRelationsJSONL(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Evidence[0].ParagraphID != "C01P007" {
		t.Fatalf("relations = %+v", got)
	}
}

func TestLoadRelationsJSONLRejectsTrailingGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relations.jsonl")
	if err := os.WriteFile(path, []byte(`{"subject":"阿Q"} trailing`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRelationsJSONL(path); err == nil {
		t.Fatal("expected malformed JSONL to fail")
	}
}

func TestLoadAliasesMapsEveryAcceptedSurface(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aliases.json")
	body := `[{"canonical_name":"阿Q","aliases":["阿Q","阿Quei","老Q"],"rejected_aliases":["阿贵"]}]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAliases(path)
	if err != nil {
		t.Fatal(err)
	}
	if got["老Q"] != "阿Q" || got["阿贵"] != "" {
		t.Fatalf("aliases = %+v", got)
	}
}

func TestLoadSnapshotRequiresSchemaAndModelDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"model":{"name":"qwen"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSnapshot(path); err == nil {
		t.Fatal("expected missing model digest to fail")
	}
}
