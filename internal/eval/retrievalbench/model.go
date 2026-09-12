package retrievalbench

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

const ManifestSchemaVersion = "miracl-zh-mini.v1"

type BenchmarkQuery struct {
	ID   string `json:"query_id"`
	Text string `json:"text"`
}
type BenchmarkDocument struct {
	SourceDocumentID string `json:"document_id"`
	Title            string `json:"title,omitempty"`
	Text             string `json:"text"`
	ContentSHA256    string `json:"content_sha256"`
	HifyDocumentID   string `json:"hify_document_id,omitempty"`
	JudgmentStatus   string `json:"judgment_status,omitempty"`
}
type Qrel struct {
	QueryID          string `json:"query_id"`
	SourceDocumentID string `json:"document_id"`
	Relevance        int    `json:"relevance"`
}

type DatasetManifest struct {
	SchemaVersion   string   `json:"schema_version"`
	Dataset         string   `json:"dataset"`
	Revision        string   `json:"revision"`
	Language        string   `json:"language"`
	Split           string   `json:"split"`
	License         string   `json:"license"`
	SourceURL       string   `json:"source_url,omitempty"`
	SelectionSeed   int64    `json:"selection_seed"`
	QueryLimit      int      `json:"query_limit"`
	TargetDocuments int      `json:"target_documents"`
	MinDocuments    int      `json:"min_documents"`
	MaxDocuments    int      `json:"max_documents"`
	QueryIDs        []string `json:"query_ids"`
	DocumentIDs     []string `json:"document_ids"`
	QrelCount       int      `json:"qrel_count"`
	QrelsSHA256     string   `json:"qrels_sha256"`
	CorpusSHA256    string   `json:"corpus_sha256"`
	ConfigSHA256    string   `json:"config_sha256"`
	CreatedAt       string   `json:"created_at,omitempty"`
}
type DatasetInput struct {
	Queries []BenchmarkQuery
	Qrels   []Qrel
	Corpus  []BenchmarkDocument
}
type PrepareOptions struct {
	Dataset, Revision, Language, Split, License, SourceURL  string
	Seed                                                    int64
	QueryLimit, TargetDocuments, MinDocuments, MaxDocuments int
}
type PreparedDataset struct {
	Manifest     DatasetManifest     `json:"manifest"`
	Queries      []BenchmarkQuery    `json:"queries"`
	Documents    []BenchmarkDocument `json:"documents"`
	Qrels        []Qrel              `json:"qrels"`
	PrepareStats PhaseStats          `json:"prepare_stats"`
}

// PhaseStats records counts and elapsed time without retaining prompts,
// document bodies, vectors, or credentials.
type PhaseStats struct {
	ElapsedMS     int64 `json:"elapsed_ms"`
	DocumentCount int   `json:"document_count"`
	ChunkCount    int   `json:"chunk_count"`
	QueryCount    int   `json:"query_count"`
}

type BenchmarkStages struct {
	Prepare       PhaseStats `json:"prepare"`
	Embedding     PhaseStats `json:"embedding"`
	Ingest        PhaseStats `json:"ingest"`
	Query         PhaseStats `json:"query"`
	EmbeddingCost string     `json:"embedding_cost"`
}

type RerankModelIdentity struct {
	ModelName  string            `json:"model_name"`
	Revision   string            `json:"revision"`
	Digest     string            `json:"digest"`
	License    string            `json:"license"`
	Runtime    map[string]string `json:"runtime"`
	EndpointID string            `json:"endpoint_id"`
	Ready      bool              `json:"ready"`
	// Source 区分本地 sidecar（空值即 local_sidecar，兼容 012 制品）与托管 API。
	Source string `json:"source,omitempty"`
}

type RerankStatsSnapshot struct {
	RequestCount        int       `json:"request_count"`
	SuccessCount        int       `json:"success_count"`
	FailureCount        int       `json:"failure_count"`
	DegradedCount       int       `json:"degraded_count"`
	CandidateCountTotal int       `json:"candidate_count_total"`
	CurrentRSSBytes     int64     `json:"current_rss_bytes"`
	PeakRSSBytes        int64     `json:"peak_rss_bytes"`
	SwapUsedBytes       int64     `json:"swap_used_bytes"`
	ColdStartMS         int64     `json:"cold_start_ms"`
	SteadyLatencyMS     []float64 `json:"steady_latency_ms,omitempty"`
}

// RerankPhaseStats 的 Hify* 字段来自 Hify observer 的真实观测。014 的托管 Rerank
// 没有 sidecar 统计：HifyP50MS/HifyP95MS 取 Hify 逐次 rerank 调用耗时，
// HifyTotalTokens 取 Voyage 成功响应 usage.total_tokens 之和；三者 omitempty，
// 保证 012 制品重算时字节不变。
type RerankPhaseStats struct {
	RequestCount        int                  `json:"request_count"`
	SuccessCount        int                  `json:"success_count"`
	FailureCount        int                  `json:"failure_count"`
	DegradedCount       int                  `json:"degraded_count"`
	CandidateCountTotal int                  `json:"candidate_count_total"`
	ColdStartMS         int64                `json:"cold_start_ms"`
	SteadyP50MS         float64              `json:"steady_p50_ms"`
	SteadyP95MS         float64              `json:"steady_p95_ms"`
	PeakRSSBytes        int64                `json:"peak_rss_bytes"`
	SwapDeltaBytes      int64                `json:"swap_delta_bytes"`
	HifyOutcome         string               `json:"hify_outcome,omitempty"`
	HifyEnabledCount    int                  `json:"hify_enabled_count"`
	HifyAppliedCount    int                  `json:"hify_applied_count"`
	HifyDegradedCount   int                  `json:"hify_degraded_count"`
	HifyInputCount      int                  `json:"hify_input_count"`
	HifyDurationMS      int64                `json:"hify_duration_ms"`
	HifyP50MS           float64              `json:"hify_p50_ms,omitempty"`
	HifyP95MS           float64              `json:"hify_p95_ms,omitempty"`
	HifyLatencyStatus   string               `json:"hify_latency_status,omitempty"`
	HifyTotalTokens     int                  `json:"hify_total_tokens,omitempty"`
	HifyFailureCounts   map[string]int       `json:"hify_failure_counts,omitempty"`
	StatsBefore         *RerankStatsSnapshot `json:"stats_before,omitempty"`
	StatsAfter          *RerankStatsSnapshot `json:"stats_after,omitempty"`
}

type ChunkHit struct {
	ChunkID    string `json:"chunk_id"`
	DocumentID string `json:"document_id"`
	Rank       int    `json:"rank"`
}
type RawQueryResult struct {
	QueryID         string     `json:"query_id"`
	ChunkHits       []ChunkHit `json:"chunk_hits,omitempty"`
	DocumentRanking []string   `json:"document_ranking,omitempty"`
	Error           string     `json:"error,omitempty"`
	ElapsedMS       int64      `json:"elapsed_ms,omitempty"`
}
type Fingerprint struct {
	Dataset               string   `json:"dataset"`
	Revision              string   `json:"revision"`
	QueryIDs              []string `json:"query_ids,omitempty"`
	DocumentIDs           []string `json:"document_ids,omitempty"`
	QrelsSHA256           string   `json:"qrels_sha256"`
	CorpusSHA256          string   `json:"corpus_sha256"`
	EmbeddingModel        string   `json:"embedding_model"`
	EmbeddingDigest       string   `json:"embedding_digest"`
	EmbeddingDimension    int      `json:"embedding_dimension"`
	SelectionConfigSHA256 string   `json:"selection_config_sha256"`
	ChunkConfig           string   `json:"chunk_config,omitempty"`
	RetrievalConfig       string   `json:"retrieval_config,omitempty"`
	ServiceEndpointID     string   `json:"service_endpoint_id"`
	K                     []int    `json:"k"`
	MetricVersion         string   `json:"metric_version"`
	CodeRevision          string   `json:"code_revision,omitempty"`
	RerankEnabled         bool     `json:"rerank_enabled,omitempty"`
	RerankModelName       string   `json:"rerank_model_name,omitempty"`
	RerankModelDigest     string   `json:"rerank_model_digest,omitempty"`
	RerankCandidateLimit  int      `json:"rerank_candidate_limit,omitempty"`
	RerankTimeoutMS       int64    `json:"rerank_timeout_ms,omitempty"`
	RunMode               string   `json:"run_mode,omitempty"`
}
type RetrievalRun struct {
	Fingerprint      Fingerprint          `json:"fingerprint"`
	Queries          []BenchmarkQuery     `json:"queries"`
	Qrels            []Qrel               `json:"qrels"`
	Results          []RawQueryResult     `json:"results"`
	Complete         bool                 `json:"complete"`
	QueryCount       int                  `json:"query_count"`
	FailedQueryCount int                  `json:"failed_query_count"`
	Stages           BenchmarkStages      `json:"stages"`
	RerankIdentity   *RerankModelIdentity `json:"rerank_identity,omitempty"`
	RerankStats      *RerankPhaseStats    `json:"rerank_stats,omitempty"`
	HostedRerankCost *HostedRerankCost    `json:"hosted_rerank_cost,omitempty"`
	RerankPacing     *RerankPacing        `json:"rerank_pacing,omitempty"`
}
type QueryMetrics struct {
	QueryID           string  `json:"query_id"`
	K                 int     `json:"k"`
	RelevantDocuments int     `json:"relevant_documents"`
	ReturnedDocuments int     `json:"returned_documents"`
	TruePositives     int     `json:"true_positives"`
	Recall            float64 `json:"recall"`
	Precision         float64 `json:"precision"`
	MRR               float64 `json:"mrr"`
	AP                float64 `json:"ap"`
	NDCG              float64 `json:"ndcg"`
}
type AggregateMetrics struct {
	K         int     `json:"k"`
	Recall    float64 `json:"recall"`
	Precision float64 `json:"precision"`
	MRR       float64 `json:"mrr"`
	MAP       float64 `json:"map"`
	NDCG      float64 `json:"ndcg"`
}
type MetricReport struct {
	Fingerprint      Fingerprint          `json:"fingerprint"`
	Queries          []QueryMetrics       `json:"queries"`
	Aggregates       []AggregateMetrics   `json:"aggregates"`
	QueryCount       int                  `json:"query_count"`
	FailedQueryCount int                  `json:"failed_query_count"`
	Complete         bool                 `json:"complete"`
	Stages           BenchmarkStages      `json:"stages"`
	RerankIdentity   *RerankModelIdentity `json:"rerank_identity,omitempty"`
	RerankStats      *RerankPhaseStats    `json:"rerank_stats,omitempty"`
	HostedRerankCost *HostedRerankCost    `json:"hosted_rerank_cost,omitempty"`
	RerankPacing     *RerankPacing        `json:"rerank_pacing,omitempty"`
}
type QueryDelta struct {
	QueryID     string  `json:"query_id"`
	K           int     `json:"k"`
	RecallDelta float64 `json:"recall_delta"`
	MRRDelta    float64 `json:"mrr_delta"`
	NDCGDelta   float64 `json:"ndcg_delta"`
}
type ComparisonReport struct {
	Status             string             `json:"status"`
	NonComparable      bool               `json:"non_comparable"`
	ExperimentVariable string             `json:"experiment_variable,omitempty"`
	Deltas             []AggregateMetrics `json:"deltas,omitempty"`
	ImprovedQueries    []string           `json:"improved_queries,omitempty"`
	RegressedQueries   []string           `json:"regressed_queries,omitempty"`
	UnchangedQueries   []string           `json:"unchanged_queries,omitempty"`
}

func (m DatasetManifest) Validate() error {
	if m.SchemaVersion == "" || len(m.QueryIDs) == 0 || len(m.DocumentIDs) == 0 {
		return fmt.Errorf("manifest is incomplete")
	}
	return nil
}
func hashBytes(b []byte) string   { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func contentHash(s string) string { return hashBytes([]byte(s)) }
func sortStrings(a []string)      { sort.Strings(a) }
func nonempty(s string) bool      { return strings.TrimSpace(s) != "" }
