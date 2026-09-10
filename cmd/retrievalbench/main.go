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
	seed                                                                int64
	queryLimit, target, minDocs, maxDocs                                int
}

func parseArgs(args []string) (commandConfig, error) {
	if len(args) < 2 {
		return commandConfig{}, errors.New("retrievalbench: subcommand is required (prepare, ingest, run, score, compare)")
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
	default:
		return fmt.Errorf("unknown subcommand %q", c.command)
	}
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
	metadataFilterEnabled bool
}

func benchmarkOllamaURL() string {
	base := os.Getenv("HIFY_BENCHMARK_OLLAMA_URL")
	if base == "" {
		base = "http://127.0.0.1:11434"
	}
	return strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1")
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
	modelName, modelDigest, err := verifyBenchmarkModel()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
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
	return &benchmarkRuntime{db: db, pgdb: pg, redis: rdb, asynq: ac, service: ks, provider: ps, embeddingModel: modelName, embeddingDigest: modelDigest, serviceEndpointID: benchmarkServiceEndpointID(benchmarkOllamaURL()), rerankEnabled: cfg.RAGRerankEnabled, metadataFilterEnabled: cfg.RAGMetadataFilterEnabled}, nil
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
	rt, err := newBenchmarkRuntime()
	if err != nil {
		return err
	}
	defer rt.Close()
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
	queryStarted := time.Now()
	for _, q := range ds.Queries {
		rr, e := a.Retrieve(context.Background(), q.Text, 10)
		r := retrievalbench.RawQueryResult{QueryID: q.ID, ElapsedMS: rr.ElapsedMS}
		for _, h := range rr.Result.ChunkHits {
			r.ChunkHits = append(r.ChunkHits, retrievalbench.ChunkHit{ChunkID: h.ChunkID, DocumentID: h.SourceDocumentID, Rank: h.Rank})
		}
		r.DocumentRanking = rr.Result.DocumentRanking
		if e != nil {
			r.Error = e.Error()
			run.FailedQueryCount++
			run.Complete = false
		}
		run.Results = append(run.Results, r)
	}
	run.Stages.Query = retrievalbench.PhaseStats{ElapsedMS: time.Since(queryStarted).Milliseconds(), DocumentCount: len(ds.Documents), ChunkCount: ingestReport.ChunkCount, QueryCount: len(run.Results)}
	return retrievalbench.SaveJSON(c.output, run)
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
	out, err := retrievalbench.CompareReports(a, b, false)
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
