package narrativeeval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

type ModelInfo struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

type RunTiming struct {
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	WallMS     int64  `json:"wall_ms"`
	ActiveMS   int64  `json:"active_ms"`
	ModelMS    int64  `json:"model_ms"`
}

type Ledger struct {
	ConfirmedCalls int     `json:"confirmed_calls"`
	PossibleCalls  int     `json:"possible_calls"`
	UnknownUsage   int     `json:"unknown_usage_attempts"`
	PromptTokens   *int64  `json:"prompt_tokens"`
	OutputTokens   *int64  `json:"output_tokens"`
	CostKind       string  `json:"cost_kind"`
	CostAmount     *string `json:"cost_amount"`
}

type Snapshot struct {
	SchemaVersion int                    `json:"schema_version"`
	ReferenceKind string                 `json:"reference_kind"`
	CorpusSHA256  string                 `json:"corpus_sha256"`
	ConfigSHA256  string                 `json:"config_sha256"`
	Model         ModelInfo              `json:"model"`
	Timing        RunTiming              `json:"timing"`
	Ledger        Ledger                 `json:"ledger"`
	Relations     []Relation             `json:"relations"`
	RawResponses  []json.RawMessage      `json:"raw_responses,omitempty"`
	Metadata      map[string]interface{} `json:"metadata,omitempty"`
}

func LoadRelationsJSONL(path string) ([]Relation, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Relation
	s := bufio.NewScanner(f)
	buf := make([]byte, 64*1024)
	s.Buffer(buf, 4*1024*1024)
	line := 0
	for s.Scan() {
		line++
		if len(bytes.TrimSpace(s.Bytes())) == 0 {
			continue
		}
		var r Relation
		if err := decodeOne(s.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if strings.TrimSpace(r.Subject) == "" || strings.TrimSpace(r.Object) == "" ||
			strings.TrimSpace(r.Type) == "" || r.Chapter < 1 {
			return nil, fmt.Errorf("%s:%d: incomplete relation", path, line)
		}
		out = append(out, r)
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func LoadAliases(path string) (map[string]string, error) {
	type aliasRecord struct {
		Canonical string   `json:"canonical_name"`
		Aliases   []string `json:"aliases"`
	}
	var records []aliasRecord
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := decodeOne(b, &records); err != nil {
		return nil, err
	}
	out := make(map[string]string)
	for _, record := range records {
		if record.Canonical == "" {
			return nil, fmt.Errorf("alias record has empty canonical_name")
		}
		out[record.Canonical] = record.Canonical
		for _, alias := range record.Aliases {
			if alias != "" {
				out[alias] = record.Canonical
			}
		}
	}
	return out, nil
}

func LoadSnapshot(path string) (Snapshot, error) {
	var snap Snapshot
	b, err := os.ReadFile(path)
	if err != nil {
		return snap, err
	}
	if err := decodeOne(b, &snap); err != nil {
		return snap, err
	}
	if snap.SchemaVersion != 1 {
		return snap, fmt.Errorf("unsupported schema_version %d", snap.SchemaVersion)
	}
	if snap.Model.Name == "" || snap.Model.Digest == "" {
		return snap, fmt.Errorf("model name and digest are required")
	}
	return snap, nil
}

func decodeOne(b []byte, dst interface{}) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra interface{}
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}
