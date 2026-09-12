// Command retrievalbench prepares and scores the local MIRACL mini dataset.
// The command deliberately has no chat-model or judge integration.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"hify/internal/config"
	"hify/internal/eval/retrievalbench"
	"hify/internal/knowledge"
	"hify/internal/platform"
	"hify/internal/provider"
)

type commandConfig struct {
	command, input, output, baseline, candidate, queries, qrels, corpus string
	rerankBaseURL, rerankModel, rerankUserID                            string
	rerankExperiment, qualityExperiment                                 bool
	runMode, rerankSource, gate                                         string
	dataset                                                             string
	seed                                                                int64
	queryLimit, target, minDocs, maxDocs                                int
	diagnosticThreshold                                                 float64
	pacingInterval                                                      time.Duration
}

type RerankHealth struct {
	Ready    bool              `json:"ready"`
	Model    string            `json:"model"`
	Revision string            `json:"revision"`
	Digest   string            `json:"digest"`
	License  string            `json:"license"`
	Runtime  map[string]string `json:"runtime"`
}

func fetchRerankStats(baseURL string) (retrievalbench.RerankStatsSnapshot, error) {
	var body retrievalbench.RerankStatsSnapshot
	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(strings.TrimRight(baseURL, "/") + "/stats")
	if err != nil {
		return body, fmt.Errorf("rerank stats: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return body, fmt.Errorf("rerank stats: HTTP %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return body, fmt.Errorf("rerank stats decode: %w", err)
	}
	return body, nil
}

func rerankStatsSettled(stats retrievalbench.RerankStatsSnapshot) bool {
	return stats.RequestCount == stats.SuccessCount+stats.FailureCount
}

func waitForRerankStats(baseURL string, maxWait time.Duration) (retrievalbench.RerankStatsSnapshot, error) {
	deadline := time.Now().Add(maxWait)
	var latest retrievalbench.RerankStatsSnapshot
	for {
		stats, err := fetchRerankStats(baseURL)
		if err != nil {
			return latest, err
		}
		latest = stats
		if rerankStatsSettled(stats) || time.Now().After(deadline) {
			return latest, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func precheckRerankService(baseURL string) (RerankHealth, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	resp, err := http.Get(baseURL + "/health")
	if err != nil {
		return RerankHealth{}, fmt.Errorf("rerank health precheck: %w", err)
	}
	defer resp.Body.Close()
	var health RerankHealth
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return health, fmt.Errorf("rerank health precheck decode: %w", err)
	}
	if resp.StatusCode != http.StatusOK || !health.Ready {
		return health, fmt.Errorf("rerank service is not ready")
	}
	if health.Model != "BAAI/bge-reranker-v2-m3" || health.Revision != "953dc6f6f85a1b2dbfca4c34a2796e7dde08d41e" || health.Digest != "d9e3e081faff1eefb84019509b2f5558fd74c1a05a2c7db22f74174fcedb5286" || health.License != "Apache-2.0" || health.Runtime["python"] == "" {
		return health, fmt.Errorf("rerank service identity does not match fixed model")
	}
	requestBody := strings.NewReader(`{"model":"BAAI/bge-reranker-v2-m3","query":"测试问题","documents":["相关内容","无关内容"]}`)
	rr, err := http.Post(baseURL+"/rerank", "application/json", requestBody)
	if err != nil {
		return health, fmt.Errorf("rerank contract precheck: %w", err)
	}
	defer rr.Body.Close()
	if rr.StatusCode != http.StatusOK {
		return health, fmt.Errorf("rerank contract precheck: HTTP %s", rr.Status)
	}
	var result struct {
		Results []struct {
			Index int     `json:"index"`
			Score float64 `json:"relevance_score"`
		} `json:"results"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		return health, fmt.Errorf("rerank contract precheck decode: %w", err)
	}
	if len(result.Results) != 2 || result.Results[0].Index == result.Results[1].Index || result.Results[0].Score == result.Results[1].Score {
		return health, fmt.Errorf("rerank contract precheck returned incomplete or constant scores")
	}
	return health, nil
}

func DiffRerankStats(before, after retrievalbench.RerankStatsSnapshot) retrievalbench.RerankPhaseStats {
	latencies := after.SteadyLatencyMS
	if len(before.SteadyLatencyMS) <= len(latencies) {
		latencies = latencies[len(before.SteadyLatencyMS):]
	}
	latencies = append([]float64(nil), latencies...)
	sort.Float64s(latencies)
	percentile := func(p float64) float64 {
		if len(latencies) == 0 {
			return 0
		}
		idx := int(p * float64(len(latencies)))
		if idx >= len(latencies) {
			idx = len(latencies) - 1
		}
		return latencies[idx]
	}
	return retrievalbench.RerankPhaseStats{
		RequestCount:        after.RequestCount - before.RequestCount,
		SuccessCount:        after.SuccessCount - before.SuccessCount,
		FailureCount:        after.FailureCount - before.FailureCount,
		DegradedCount:       after.DegradedCount - before.DegradedCount,
		CandidateCountTotal: after.CandidateCountTotal - before.CandidateCountTotal,
		ColdStartMS:         after.ColdStartMS - before.ColdStartMS,
		SteadyP50MS:         percentile(0.5),
		SteadyP95MS:         percentile(0.95),
		PeakRSSBytes:        after.PeakRSSBytes,
		SwapDeltaBytes:      after.SwapUsedBytes - before.SwapUsedBytes,
		StatsBefore:         &before,
		StatsAfter:          &after,
	}
}

func ensureRerankProviderModel(ctx context.Context, svc provider.Service, baseURL, userID, modelName string) (provider.Model, error) {
	providers, _, err := svc.ListProviders(ctx, 200, 0)
	if err != nil {
		return provider.Model{}, err
	}
	var target provider.Provider
	for _, p := range providers {
		if p.IsActive && p.BaseURL == baseURL && p.AuthType == provider.AuthTypeNone {
			target = p
			break
		}
	}
	if target.ID == "" {
		target, err = svc.CreateProvider(ctx, provider.CreateProviderInput{
			Name: "hify-rerank-local", AdapterType: provider.AdapterOpenAICompatible,
			BaseURL: baseURL, AuthType: provider.AuthTypeNone, CreatedBy: userID,
			ExtraConfig: provider.ExtraConfig{MaxConcurrent: 2, RateLimitPerMinute: 1000},
		})
		if err != nil {
			return provider.Model{}, err
		}
	}
	models, err := svc.ListModels(ctx, target.ID)
	if err != nil {
		return provider.Model{}, err
	}
	for _, m := range models {
		if m.IsActive && m.Capability == provider.CapabilityRerank && m.ModelName == modelName {
			return m, nil
		}
	}
	return svc.AddModel(ctx, target.ID, provider.CreateModelInput{ModelName: modelName, Capability: provider.CapabilityRerank, IsDefault: true})
}

func parseArgs(args []string) (commandConfig, error) {
	if len(args) < 2 {
		return commandConfig{}, errors.New("retrievalbench: subcommand is required (prepare, ingest, run, score, compare, setup-rerank, decision, quality-decision, gate-decision, diagnostic)")
	}
	c := commandConfig{command: args[1]}
	fs := flag.NewFlagSet(c.command, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&c.input, "input", "", "input JSON file")
	fs.StringVar(&c.output, "output", "", "output JSON file")
	fs.StringVar(&c.baseline, "baseline", "", "baseline report JSON")
	fs.StringVar(&c.candidate, "candidate", "", "candidate report JSON")
	fs.StringVar(&c.queries, "queries", "", "topics JSON file")
	fs.StringVar(&c.qrels, "qrels", "", "qrels JSON file")
	fs.StringVar(&c.corpus, "corpus", "", "corpus JSON file")
	fs.StringVar(&c.rerankBaseURL, "base-url", "", "local rerank service base URL")
	fs.StringVar(&c.rerankModel, "model", "BAAI/bge-reranker-v2-m3", "fixed rerank model name")
	fs.StringVar(&c.rerankUserID, "user-id", "", "Hify user ID for provider ownership")
	fs.BoolVar(&c.rerankExperiment, "rerank-experiment", false, "compare as the single-variable rerank experiment")
	fs.BoolVar(&c.qualityExperiment, "quality-experiment", false, "compare as the quality diagnostic experiment")
	fs.StringVar(&c.runMode, "run-mode", "deployment_gate", "benchmark run mode: deployment_gate or quality_diagnostic")
	fs.StringVar(&c.rerankSource, "rerank-source", "local_sidecar", "rerank evidence source: local_sidecar or hosted_api")
	fs.StringVar(&c.gate, "gate", "", "gate-decision gate: deployment_gate or quality_diagnostic")
	fs.DurationVar(&c.pacingInterval, "pacing-interval", 0, "benchmark-only minimum interval between query starts (hosted_api quality_diagnostic only)")
	fs.StringVar(&c.dataset, "dataset", "", "prepared dataset directory for diagnostics")
	fs.Float64Var(&c.diagnosticThreshold, "threshold", 0.5, "recall threshold for low-score diagnostics")
	fs.Int64Var(&c.seed, "seed", 11, "deterministic selection seed")
	fs.IntVar(&c.queryLimit, "query-limit", 50, "query count")
	fs.IntVar(&c.target, "target-docs", 800, "target document count")
	fs.IntVar(&c.minDocs, "min-docs", 500, "minimum documents")
	fs.IntVar(&c.maxDocs, "max-docs", 1000, "maximum documents")
	if err := fs.Parse(args[2:]); err != nil {
		return commandConfig{}, err
	}
	return c, nil
}

func main() {
	cfg, err := parseArgs(os.Args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err = run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "retrievalbench:", err)
		os.Exit(1)
	}
}

func run(c commandConfig) error {
	switch c.command {
	case "prepare":
		return prepare(c)
	case "score":
		return score(c)
	case "compare":
		return compare(c)
	case "ingest":
		return ingest(c)
	case "run":
		return runQueries(c)
	case "setup-rerank":
		return setupRerank(c)
	case "decision":
		return decision(c)
	case "quality-decision":
		return qualityDecision(c)
	case "gate-decision":
		return gateDecision(c)
	case "diagnostic":
		return diagnostic(c)
	default:
		return fmt.Errorf("unknown subcommand %q", c.command)
	}
}

type rerankCheckpoint struct {
	ProviderID string `json:"provider_id"`
	ModelID    string `json:"model_id"`
	ModelName  string `json:"model_name"`
	Revision   string `json:"revision"`
	EndpointID string `json:"endpoint_id"`
}

func setupRerank(c commandConfig) error {
	if c.rerankBaseURL == "" || c.rerankUserID == "" || c.output == "" {
		return errors.New("setup-rerank requires --base-url, --user-id and --output")
	}
	if c.rerankModel != "BAAI/bge-reranker-v2-m3" {
		return errors.New("setup-rerank only supports the fixed BAAI/bge-reranker-v2-m3 model")
	}
	health, err := precheckRerankService(c.rerankBaseURL)
	if err != nil {
		return err
	}
	rt, err := newBenchmarkRuntime()
	if err != nil {
		return err
	}
	defer rt.Close()
	m, err := ensureRerankProviderModel(context.Background(), rt.provider, strings.TrimRight(c.rerankBaseURL, "/"), c.rerankUserID, c.rerankModel)
	if err != nil {
		return err
	}
	return retrievalbench.SaveJSON(c.output, rerankCheckpoint{ProviderID: m.ProviderID, ModelID: m.ID, ModelName: health.Model, Revision: health.Revision, EndpointID: benchmarkServiceEndpointID(c.rerankBaseURL)})
}

type rerankDecisionReport struct {
	Comparison retrievalbench.ComparisonReport `json:"comparison"`
	Decision   retrievalbench.RerankDecision   `json:"decision"`
}

func decision(c commandConfig) error {
	if c.baseline == "" || c.candidate == "" || c.output == "" {
		return errors.New("decision requires --baseline, --candidate and --output")
	}
	var baseline, candidate retrievalbench.MetricReport
	if err := readJSON(c.baseline, &baseline); err != nil {
		return err
	}
	if err := readJSON(c.candidate, &candidate); err != nil {
		return err
	}
	comparison, err := retrievalbench.CompareRerankExperiment(baseline, candidate)
	if err != nil {
		return err
	}
	result := rerankDecisionReport{Comparison: comparison, Decision: retrievalbench.DecideRerank(retrievalbench.RerankDecisionInput{Baseline: baseline, Candidate: candidate, Comparable: !comparison.NonComparable})}
	return retrievalbench.SaveJSON(c.output, result)
}

func qualityDecision(c commandConfig) error {
	if c.baseline == "" || c.candidate == "" || c.output == "" {
		return errors.New("quality-decision requires --baseline, --candidate and --output")
	}
	var baseline, candidate retrievalbench.MetricReport
	if err := readJSON(c.baseline, &baseline); err != nil {
		return err
	}
	if err := readJSON(c.candidate, &candidate); err != nil {
		return err
	}
	comparison, err := retrievalbench.CompareQualityExperiment(baseline, candidate)
	if err != nil {
		return err
	}
	result := struct {
		Comparison retrievalbench.ComparisonReport `json:"comparison"`
		Decision   retrievalbench.QualityDecision  `json:"decision"`
	}{Comparison: comparison, Decision: retrievalbench.DecideQuality(retrievalbench.QualityDecisionInput{Baseline: baseline, Candidate: candidate, Comparable: !comparison.NonComparable})}
	return retrievalbench.SaveJSON(c.output, result)
}

func diagnostic(c commandConfig) error {
	if c.input == "" || c.dataset == "" || c.output == "" {
		return errors.New("diagnostic requires --input run, --dataset directory and --output")
	}
	run, err := retrievalbench.LoadRun(c.input)
	if err != nil {
		return err
	}
	dataset, err := loadDataset(c.dataset)
	if err != nil {
		return err
	}
	return retrievalbench.SaveJSON(c.output, retrievalbench.DiagnoseLowScoreQueries(dataset, run, c.diagnosticThreshold))
}

func prepare(c commandConfig) error {
	if c.output == "" {
		return errors.New("--output is required")
	}
	var in retrievalbench.DatasetInput
	if c.input != "" {
		if err := readJSON(c.input, &in); err != nil {
			return err
		}
	} else {
		if c.queries == "" || c.qrels == "" || c.corpus == "" {
			return errors.New("prepare requires --input or --queries, --qrels and --corpus")
		}
		if strings.HasSuffix(c.queries, ".tsv") && strings.HasSuffix(c.qrels, ".tsv") {
			ds, err := retrievalbench.PrepareMIRACLFiles(c.queries, c.qrels, strings.Split(c.corpus, ","), retrievalbench.PrepareOptions{Dataset: "miracl", Revision: "v1.0", Language: "zh", Split: "dev", License: "Apache-2.0", Seed: c.seed, QueryLimit: c.queryLimit, TargetDocuments: c.target, MinDocuments: c.minDocs, MaxDocuments: c.maxDocs})
			if err != nil {
				return err
			}
			return retrievalbench.WriteDataset(c.output, ds)
		}
		if err := readJSON(c.queries, &in.Queries); err != nil {
			return err
		}
		if err := readJSON(c.qrels, &in.Qrels); err != nil {
			return err
		}
		if err := readJSON(c.corpus, &in.Corpus); err != nil {
			return err
		}
	}
	ds, err := retrievalbench.PrepareDataset(in, retrievalbench.PrepareOptions{Dataset: "miracl", Revision: "v1.0", Language: "zh", Split: "dev", License: "Apache-2.0", Seed: c.seed, QueryLimit: c.queryLimit, TargetDocuments: c.target, MinDocuments: c.minDocs, MaxDocuments: c.maxDocs})
	if err != nil {
		return err
	}
	return retrievalbench.WriteDataset(c.output, ds)
}

func downloadFile(url, path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %s", url, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	return os.Rename(tmp, path)
}
func score(c commandConfig) error {
	if c.input == "" || c.output == "" {
		return errors.New("score requires --input and --output")
	}
	r, err := retrievalbench.LoadRun(c.input)
	if err != nil {
		return err
	}
	m, err := retrievalbench.RescoreRun(r)
	if err != nil {
		return err
	}
	return retrievalbench.SaveJSON(c.output, m)
}

type benchmarkRuntime struct {
	db, pgdb              interface{ Close() error }
	redis                 interface{ Close() error }
	asynq                 *asynq.Client
	service               knowledge.Service
	provider              provider.Service
	embeddingModel        string
	embeddingDigest       string
	serviceEndpointID     string
	rerankEnabled         bool
	rerankModelID         string
	rerankTimeout         time.Duration
	metadataFilterEnabled bool
}

func benchmarkOllamaURL() string {
	base := os.Getenv("HIFY_BENCHMARK_OLLAMA_URL")
	if base == "" {
		base = "http://127.0.0.1:11434"
	}
	return strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1")
}

func benchmarkRerankURL() string {
	base := os.Getenv("HIFY_BENCHMARK_RERANK_URL")
	if base == "" {
		base = "http://127.0.0.1:8090"
	}
	return strings.TrimRight(base, "/")
}

func benchmarkServiceEndpointID(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return fmt.Sprintf("sha256:%x", sum[:8])
}

func buildBenchmarkFingerprint(ds retrievalbench.PreparedDataset, model, digest, endpoint, chunkConfig, retrievalConfig string) retrievalbench.Fingerprint {
	return retrievalbench.Fingerprint{
		Dataset: ds.Manifest.Dataset, Revision: ds.Manifest.Revision,
		QueryIDs: ds.Manifest.QueryIDs, DocumentIDs: ds.Manifest.DocumentIDs,
		QrelsSHA256: ds.Manifest.QrelsSHA256, CorpusSHA256: ds.Manifest.CorpusSHA256,
		SelectionConfigSHA256: ds.Manifest.ConfigSHA256,
		EmbeddingModel:        model, EmbeddingDigest: digest, EmbeddingDimension: 1024,
		ChunkConfig: chunkConfig, RetrievalConfig: retrievalConfig,
		ServiceEndpointID: benchmarkServiceEndpointID(endpoint),
		K:                 []int{1, 3, 5, 10}, MetricVersion: "v1",
	}
}

func benchmarkChunkConfig(kb knowledge.KnowledgeBase) string {
	b, _ := json.Marshal(struct {
		ChunkSize    int `json:"chunk_size"`
		ChunkOverlap int `json:"chunk_overlap"`
	}{kb.ChunkSize, kb.ChunkOverlap})
	return string(b)
}

func benchmarkRetrievalConfig(rt *benchmarkRuntime) string {
	b, _ := json.Marshal(struct {
		TopK                  int  `json:"top_k"`
		RerankEnabled         bool `json:"rerank_enabled"`
		MetadataFilterEnabled bool `json:"metadata_filter_enabled"`
	}{10, rt.rerankEnabled, rt.metadataFilterEnabled})
	return string(b)
}

func verifyBenchmarkModel() (string, string, error) {
	name := os.Getenv("HIFY_BENCHMARK_EMBEDDING_MODEL_NAME")
	if name == "" {
		name = "bge-m3:567m"
	}
	base := benchmarkOllamaURL()
	resp, err := http.Get(base + "/api/tags")
	if err != nil {
		return "", "", fmt.Errorf("ollama model precheck: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("ollama model precheck: HTTP %s", resp.Status)
	}
	var body struct {
		Models []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", fmt.Errorf("ollama model precheck decode: %w", err)
	}
	for _, m := range body.Models {
		if m.Name == name {
			return name, m.Digest, nil
		}
	}
	return "", "", fmt.Errorf("ollama model %q is not installed", name)
}

func newBenchmarkRuntime() (*benchmarkRuntime, error) {
	return newBenchmarkRuntimeForMode("deployment_gate")
}

func newBenchmarkRuntimeForMode(runMode string) (*benchmarkRuntime, error) {
	mode, err := benchmarkRunMode(runMode)
	if err != nil {
		return nil, err
	}
	modelName, modelDigest, err := verifyBenchmarkModel()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if mode == "quality_diagnostic" {
		cfg.RAGRerankEnabled = true
		cfg.RAGRerankTimeout = benchmarkRerankTimeout(mode, cfg.RAGRerankTimeout)
	}
	db, err := platform.NewMySQLPool(cfg.MySQLDSN)
	if err != nil {
		return nil, err
	}
	pg, err := platform.NewPostgresPool(cfg.PostgresDSN)
	if err != nil {
		db.Close()
		return nil, err
	}
	rdb := platform.NewRedisClient(platform.RedisConfig{Addr: cfg.RedisAddr, Password: cfg.RedisPassword, DB: cfg.RedisDB})
	ac := platform.NewAsynqClient(platform.RedisConfig{Addr: cfg.RedisAddr, Password: cfg.RedisPassword, DB: cfg.RedisDB})
	ps, err := provider.NewService(provider.NewRepository(db), cfg.EncryptionKey, rdb)
	if err != nil {
		db.Close()
		pg.Close()
		rdb.Close()
		ac.Close()
		return nil, err
	}
	ks := knowledge.NewService(knowledge.NewRepository(db, pg), ps, ac, cfg.KnowledgeStorageDir, cfg.RAGRerankEnabled, cfg.RAGRerankModelID, cfg.RAGRerankTimeout, cfg.RAGMetadataFilterEnabled)
	return &benchmarkRuntime{db: db, pgdb: pg, redis: rdb, asynq: ac, service: ks, provider: ps, embeddingModel: modelName, embeddingDigest: modelDigest, serviceEndpointID: benchmarkServiceEndpointID(benchmarkOllamaURL()), rerankEnabled: cfg.RAGRerankEnabled, rerankModelID: cfg.RAGRerankModelID, rerankTimeout: cfg.RAGRerankTimeout, metadataFilterEnabled: cfg.RAGMetadataFilterEnabled}, nil
}

func benchmarkRunMode(mode string) (string, error) {
	if mode == "" {
		return "deployment_gate", nil
	}
	if mode != "deployment_gate" && mode != "quality_diagnostic" {
		return "", fmt.Errorf("unsupported benchmark run mode %q", mode)
	}
	return mode, nil
}

func benchmarkRerankTimeout(mode string, configured time.Duration) time.Duration {
	if mode == "quality_diagnostic" {
		return 30 * time.Second
	}
	return configured
}
func (r *benchmarkRuntime) Close() { r.asynq.Close(); r.redis.Close(); r.pgdb.Close(); r.db.Close() }
func loadDataset(dir string) (retrievalbench.PreparedDataset, error) {
	var d retrievalbench.PreparedDataset
	if err := readJSON(filepath.Join(dir, "manifest.json"), &d.Manifest); err != nil {
		return d, err
	}
	if err := readJSON(filepath.Join(dir, "queries.json"), &d.Queries); err != nil {
		return d, err
	}
	if err := readJSON(filepath.Join(dir, "documents.json"), &d.Documents); err != nil {
		return d, err
	}
	if err := readJSON(filepath.Join(dir, "qrels.json"), &d.Qrels); err != nil {
		return d, err
	}
	// Older prepared directories did not have this optional timing file; they
	// remain readable but cannot claim prepare-stage timing evidence.
	_ = readJSON(filepath.Join(dir, "prepare_stats.json"), &d.PrepareStats)
	return d, nil
}
func makeAdapter(rt *benchmarkRuntime, kbID string) (*knowledge.BenchmarkAdapter, error) {
	userID := os.Getenv("HIFY_BENCHMARK_USER_ID")
	if userID == "" {
		return nil, errors.New("HIFY_BENCHMARK_USER_ID is required")
	}
	if kbID == "" {
		name := os.Getenv("HIFY_BENCHMARK_EMBEDDING_MODEL_NAME")
		if name == "" {
			name = "bge-m3:567m"
		}
		models, err := rt.provider.ListModelsByCapability(context.Background(), provider.CapabilityEmbedding)
		if err != nil {
			return nil, err
		}
		for _, m := range models {
			if m.ModelName == name && m.IsActive {
				kb, e := rt.service.CreateKnowledgeBase(context.Background(), knowledge.CreateKnowledgeBaseInput{Name: "MIRACL 中文 Mini", Description: "011 benchmark", EmbeddingModelID: m.ID, ChunkSize: 500, ChunkOverlap: 50, CreatedBy: userID})
				if e != nil {
					return nil, e
				}
				kbID = kb.ID
				break
			}
		}
		if kbID == "" {
			return nil, fmt.Errorf("embedding model %q is not configured", name)
		}
	}
	return knowledge.NewBenchmarkAdapter(rt.service, kbID, userID, "admin"), nil
}
func ingest(c commandConfig) error {
	if c.input == "" || c.output == "" {
		return errors.New("ingest requires --input dataset directory and --output report")
	}
	ds, err := loadDataset(c.input)
	if err != nil {
		return err
	}
	rt, err := newBenchmarkRuntime()
	if err != nil {
		return err
	}
	defer rt.Close()
	var previous knowledge.BenchmarkIngestReport
	_ = readJSON(c.output, &previous) // absent checkpoint is the normal first run
	kbID := os.Getenv("HIFY_BENCHMARK_KB_ID")
	if kbID == "" {
		kbID = previous.KnowledgeBaseID
	}
	a, err := makeAdapter(rt, kbID)
	if err != nil {
		return err
	}
	ins := make([]knowledge.BenchmarkDocumentInput, 0, len(ds.Documents))
	for _, d := range ds.Documents {
		ins = append(ins, knowledge.BenchmarkDocumentInput{SourceDocumentID: d.SourceDocumentID, FileName: d.SourceDocumentID + ".txt", Content: d.Text, HifyDocumentID: previous.Documents[d.SourceDocumentID]})
	}
	rep, ingestErr := a.Ingest(context.Background(), ins)
	if err := retrievalbench.SaveJSON(c.output, rep); err != nil {
		return err
	}
	if ingestErr != nil {
		return ingestErr
	}
	return nil
}
func runQueries(c commandConfig) error {
	if c.input == "" || c.output == "" {
		return errors.New("run requires --input dataset directory and --output raw run")
	}
	ds, err := loadDataset(c.input)
	if err != nil {
		return err
	}
	runMode, err := benchmarkRunMode(c.runMode)
	if err != nil {
		return err
	}
	rerankSource, err := benchmarkRerankSource(c.rerankSource)
	if err != nil {
		return err
	}
	hosted := rerankSource == retrievalbench.RerankSourceHostedAPI
	if err := validateRerankPacing(rerankSource, runMode, c.pacingInterval); err != nil {
		return err
	}
	if hosted {
		// 014 FR-013：付费运行最多 50 条 query，raw run 不覆盖、不自动重跑。
		if len(ds.Queries) > 50 {
			return fmt.Errorf("hosted rerank run is limited to 50 queries, got %d", len(ds.Queries))
		}
		if err := ensureFreshOutput(c.output); err != nil {
			return err
		}
	}
	rt, err := newBenchmarkRuntimeForMode(runMode)
	if err != nil {
		return err
	}
	defer rt.Close()
	if hosted && !rt.rerankEnabled {
		return errors.New("hosted rerank run requires HIFY_RAG_RERANK_ENABLED=true and HIFY_RAG_RERANK_MODEL_ID")
	}
	var rerankHealth RerankHealth
	var hostedIdentity retrievalbench.RerankModelIdentity
	var rerankBefore retrievalbench.RerankStatsSnapshot
	hifyRerankEnabledCount, hifyRerankAppliedCount, hifyRerankDegradedCount := 0, 0, 0
	hifyRerankInputCount, hifyRerankDurationMS, hifyRerankTotalTokens := 0, int64(0), 0
	var rerankObservations []knowledge.BenchmarkRetrievalResult
	if rt.rerankEnabled {
		if err := validateGateRerankTimeout(runMode, rt.rerankTimeout); err != nil {
			return err
		}
	}
	if rt.rerankEnabled && hosted {
		hostedIdentity, err = precheckHostedRerank(context.Background(), rt.provider, rt.rerankModelID)
		if err != nil {
			return err
		}
	} else if rt.rerankEnabled {
		rerankHealth, err = precheckRerankService(benchmarkRerankURL())
		if err != nil {
			return err
		}
		rerankBefore, err = fetchRerankStats(benchmarkRerankURL())
		if err != nil {
			return err
		}
	}
	var ingestReport knowledge.BenchmarkIngestReport
	if err := readJSON(filepath.Join(c.input, "ingest.json"), &ingestReport); err != nil {
		return err
	}
	a, err := makeAdapter(rt, ingestReport.KnowledgeBaseID)
	if err != nil {
		return err
	}
	ins := make([]knowledge.BenchmarkDocumentInput, 0, len(ds.Documents))
	for _, d := range ds.Documents {
		hifyID := ingestReport.Documents[d.SourceDocumentID]
		if hifyID == "" {
			return errors.New("ingest report misses hify document mapping; run ingest first")
		}
		ins = append(ins, knowledge.BenchmarkDocumentInput{SourceDocumentID: d.SourceDocumentID, FileName: d.SourceDocumentID + ".txt", Content: d.Text, HifyDocumentID: hifyID})
	}
	if _, err := a.Ingest(context.Background(), ins); err != nil {
		return err
	}
	kb, err := rt.service.GetKnowledgeBase(context.Background(), ingestReport.KnowledgeBaseID)
	if err != nil {
		return err
	}
	run := retrievalbench.RetrievalRun{
		Queries: ds.Queries, Qrels: ds.Qrels, Complete: true, QueryCount: len(ds.Queries),
		Fingerprint: buildBenchmarkFingerprint(ds, rt.embeddingModel, rt.embeddingDigest, benchmarkOllamaURL(), benchmarkChunkConfig(kb), benchmarkRetrievalConfig(rt)),
		Stages: retrievalbench.BenchmarkStages{
			Prepare:       ds.PrepareStats,
			Embedding:     retrievalbench.PhaseStats{ElapsedMS: ingestReport.EmbeddingElapsedMS, DocumentCount: ingestReport.EmbeddingDocumentCount, ChunkCount: ingestReport.EmbeddingChunkCount},
			Ingest:        retrievalbench.PhaseStats{ElapsedMS: ingestReport.ElapsedMS, DocumentCount: ingestReport.Ready + ingestReport.Failed, ChunkCount: ingestReport.ChunkCount},
			EmbeddingCost: "not_applicable",
		},
	}
	run.Fingerprint.RunMode = runMode
	if rt.rerankEnabled && hosted {
		applyRerankFingerprint(&run.Fingerprint, hostedIdentity.ModelName, hostedIdentity.EndpointID, runMode, rt.rerankTimeout)
		run.RerankIdentity = &hostedIdentity
	} else if rt.rerankEnabled {
		applyRerankFingerprint(&run.Fingerprint, rerankHealth.Model, rerankHealth.Revision, runMode, rt.rerankTimeout)
		run.RerankIdentity = &retrievalbench.RerankModelIdentity{ModelName: rerankHealth.Model, Revision: rerankHealth.Revision, Digest: rerankHealth.Digest, License: rerankHealth.License, Runtime: rerankHealth.Runtime, EndpointID: benchmarkServiceEndpointID(benchmarkRerankURL()), Ready: rerankHealth.Ready}
	}
	queryStarted := time.Now()
	var lastQueryStart time.Time
	var pacingWaited time.Duration
	for _, q := range ds.Queries {
		// 014 FR-015：benchmark-only 限速，只拉开相邻 query 的起点，不改变检索与 Rerank。
		if wait := paceWait(lastQueryStart, time.Now(), c.pacingInterval); wait > 0 {
			time.Sleep(wait)
			pacingWaited += wait
		}
		lastQueryStart = time.Now()
		rr, e := a.Retrieve(context.Background(), q.Text, 10)
		r := retrievalbench.RawQueryResult{QueryID: q.ID, ElapsedMS: rr.ElapsedMS}
		for _, h := range rr.Result.ChunkHits {
			r.ChunkHits = append(r.ChunkHits, retrievalbench.ChunkHit{ChunkID: h.ChunkID, DocumentID: h.SourceDocumentID, Rank: h.Rank})
		}
		r.DocumentRanking = rr.Result.DocumentRanking
		if rr.RerankEnabled {
			hifyRerankEnabledCount++
		}
		if rr.RerankApplied {
			hifyRerankAppliedCount++
		}
		if rr.RerankDegraded {
			hifyRerankDegradedCount++
		}
		hifyRerankInputCount += rr.RerankInputCount
		hifyRerankDurationMS += rr.RerankDurationMS
		hifyRerankTotalTokens += rr.RerankTotalTokens
		rerankObservations = append(rerankObservations, rr)
		if e != nil {
			// Raw hosted runs must not persist provider error strings, which may
			// contain upstream response bodies. Keep only a safe scoring marker;
			// the ordinary local sidecar path retains its existing error semantics.
			r.Error = runQueryErrorMarker(hosted, e)
			run.FailedQueryCount++
			run.Complete = false
		}
		run.Results = append(run.Results, r)
	}
	run.RerankPacing = rerankPacingRecord(c.pacingInterval, pacingWaited)
	run.Stages.Query = retrievalbench.PhaseStats{ElapsedMS: time.Since(queryStarted).Milliseconds(), DocumentCount: len(ds.Documents), ChunkCount: ingestReport.ChunkCount, QueryCount: len(run.Results)}
	if rt.rerankEnabled && hosted {
		run.RerankStats = hostedRerankPhaseStats(len(ds.Queries), hifyRerankEnabledCount, hifyRerankAppliedCount, hifyRerankDegradedCount, hifyRerankInputCount, hifyRerankDurationMS, successfulRerankDurations(rerankObservations), hifyRerankTotalTokens)
		run.RerankStats.HifyFailureCounts = aggregateHostedFailureKinds(rerankObservations)
		cost, costErr := retrievalbench.NewVoyageRerankCost(hifyRerankTotalTokens, queryStarted.UTC().Format(time.RFC3339))
		if costErr != nil {
			return costErr
		}
		run.HostedRerankCost = &cost
	} else if rt.rerankEnabled {
		rerankAfter, statsErr := waitForRerankStats(benchmarkRerankURL(), 20*time.Second)
		if statsErr != nil {
			return statsErr
		}
		run.RerankStats = rerankPhaseStats(rerankBefore, rerankAfter, len(ds.Queries), hifyRerankEnabledCount, hifyRerankAppliedCount, hifyRerankDegradedCount, hifyRerankInputCount, hifyRerankDurationMS)
	}
	if run.RerankStats != nil && (run.RerankStats.HifyAppliedCount != len(ds.Queries) || run.RerankStats.HifyDegradedCount != 0) {
		run.Complete = false
	}
	if hosted {
		// Hosted raw evidence may contain query IDs only. Sanitize immediately
		// before persistence so the normal MIRACL/BGE path remains unchanged.
		sanitized, sanitizeErr := retrievalbench.SanitizeHostedRun(run)
		if sanitizeErr != nil {
			return sanitizeErr
		}
		run = sanitized
	}
	return retrievalbench.SaveJSON(c.output, run)
}

func rerankPhaseStats(before, after retrievalbench.RerankStatsSnapshot, queryCount, hifyEnabledCount, hifyAppliedCount, hifyDegradedCount, hifyInputCount int, hifyDurationMS int64) *retrievalbench.RerankPhaseStats {
	stats := DiffRerankStats(before, after)
	stats.HifyEnabledCount = hifyEnabledCount
	stats.HifyAppliedCount = hifyAppliedCount
	stats.HifyDegradedCount = hifyDegradedCount
	stats.HifyInputCount = hifyInputCount
	stats.HifyDurationMS = hifyDurationMS
	if hifyEnabledCount == queryCount && hifyAppliedCount == queryCount && hifyDegradedCount == 0 {
		stats.HifyOutcome = "all_applied"
	} else {
		stats.HifyOutcome = "not_all_applied_or_degraded"
	}
	return &stats
}
func compare(c commandConfig) error {
	if c.baseline == "" || c.candidate == "" {
		return errors.New("compare requires --baseline and --candidate")
	}
	var a, b retrievalbench.MetricReport
	if err := readJSON(c.baseline, &a); err != nil {
		return err
	}
	if err := readJSON(c.candidate, &b); err != nil {
		return err
	}
	var out retrievalbench.ComparisonReport
	var err error
	if c.qualityExperiment {
		out, err = retrievalbench.CompareQualityExperiment(a, b)
	} else if c.rerankExperiment {
		out, err = retrievalbench.CompareRerankExperiment(a, b)
	} else {
		out, err = retrievalbench.CompareReports(a, b, false)
	}
	if err != nil {
		return err
	}
	if c.output == "" {
		fmt.Printf("%s\n", out.Status)
		return nil
	}
	return retrievalbench.SaveJSON(c.output, out)
}
func readJSON(path string, v any) error {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
