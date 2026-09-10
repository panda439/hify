package retrievalbench

import (
	"encoding/json"
	"fmt"
	"os"
)

func SaveJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
func LoadRun(path string) (RetrievalRun, error) {
	var r RetrievalRun
	b, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("decode run: %w", err)
	}
	return r, nil
}
func RescoreRun(r RetrievalRun) (MetricReport, error) {
	report, err := ScoreRun(r.Queries, r.Results, r.Qrels, r.Fingerprint.K)
	if err != nil {
		return MetricReport{}, err
	}
	report.Fingerprint = r.Fingerprint
	report.Stages = r.Stages
	report.Complete = r.Complete && report.FailedQueryCount == 0
	return report, nil
}
