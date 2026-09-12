package retrievalbench

import (
	"bufio"
	"compress/gzip"
	"container/heap"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func PrepareDataset(in DatasetInput, opts PrepareOptions) (PreparedDataset, error) {
	started := time.Now()
	if opts.QueryLimit <= 0 {
		opts.QueryLimit = 50
	}
	if opts.TargetDocuments <= 0 {
		opts.TargetDocuments = 800
	}
	if opts.MinDocuments <= 0 {
		opts.MinDocuments = 500
	}
	if opts.MaxDocuments <= 0 {
		opts.MaxDocuments = 1000
	}
	queries := make(map[string]BenchmarkQuery)
	for _, q := range in.Queries {
		if q.ID == "" || !nonempty(q.Text) {
			return PreparedDataset{}, fmt.Errorf("invalid query")
		}
		if _, ok := queries[q.ID]; ok {
			return PreparedDataset{}, fmt.Errorf("duplicate query %s", q.ID)
		}
		queries[q.ID] = q
	}
	corpus := make(map[string]BenchmarkDocument)
	for _, d := range in.Corpus {
		if d.SourceDocumentID == "" || !nonempty(d.Text) {
			return PreparedDataset{}, fmt.Errorf("invalid document")
		}
		if old, ok := corpus[d.SourceDocumentID]; ok && old.Text != d.Text {
			return PreparedDataset{}, fmt.Errorf("conflicting document %s", d.SourceDocumentID)
		}
		corpus[d.SourceDocumentID] = d
	}
	qrelsByQuery := make(map[string][]Qrel)
	for _, q := range in.Qrels {
		if _, ok := queries[q.QueryID]; !ok {
			return PreparedDataset{}, fmt.Errorf("qrel references missing query %s", q.QueryID)
		}
		qrelsByQuery[q.QueryID] = append(qrelsByQuery[q.QueryID], q)
	}
	type candidate struct {
		id  string
		key string
	}
	cs := make([]candidate, 0, len(queries))
	for id := range queries {
		pos := false
		for _, q := range qrelsByQuery[id] {
			if q.Relevance > 0 {
				pos = true
			}
		}
		if pos {
			cs = append(cs, candidate{id, hashBytes([]byte(strconv.FormatInt(opts.Seed, 10) + "/" + id))})
		}
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].key == cs[j].key {
			return cs[i].id < cs[j].id
		}
		return cs[i].key < cs[j].key
	})
	if len(cs) < opts.QueryLimit {
		return PreparedDataset{}, fmt.Errorf("only %d queries with positive qrels", len(cs))
	}
	selected := cs[:opts.QueryLimit]
	selectedIDs := make(map[string]struct{}, len(selected))
	outQueries := make([]BenchmarkQuery, 0, len(selected))
	for _, c := range selected {
		selectedIDs[c.id] = struct{}{}
		outQueries = append(outQueries, queries[c.id])
	}
	sort.Slice(outQueries, func(i, j int) bool { return outQueries[i].ID < outQueries[j].ID })
	qrels := make([]Qrel, 0)
	required := make(map[string]struct{})
	for _, q := range in.Qrels {
		if _, ok := selectedIDs[q.QueryID]; !ok {
			continue
		}
		if _, ok := corpus[q.SourceDocumentID]; !ok {
			return PreparedDataset{}, fmt.Errorf("qrel references missing document %s", q.SourceDocumentID)
		}
		qrels = append(qrels, q)
		required[q.SourceDocumentID] = struct{}{}
	}
	sort.Slice(qrels, func(i, j int) bool {
		if qrels[i].QueryID == qrels[j].QueryID {
			return qrels[i].SourceDocumentID < qrels[j].SourceDocumentID
		}
		return qrels[i].QueryID < qrels[j].QueryID
	})
	for i := 1; i < len(qrels); i++ {
		if qrels[i].QueryID == qrels[i-1].QueryID && qrels[i].SourceDocumentID == qrels[i-1].SourceDocumentID && qrels[i].Relevance != qrels[i-1].Relevance {
			return PreparedDataset{}, fmt.Errorf("conflicting qrel %s/%s", qrels[i].QueryID, qrels[i].SourceDocumentID)
		}
	}
	if len(required) > opts.MaxDocuments {
		return PreparedDataset{}, fmt.Errorf("judged documents %d exceed max %d", len(required), opts.MaxDocuments)
	}
	ids := make([]string, 0, len(required))
	for id := range required {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	docs := make([]BenchmarkDocument, 0, opts.TargetDocuments)
	for _, id := range ids {
		d := corpus[id]
		d.ContentSHA256 = contentHash(d.Text)
		d.JudgmentStatus = "judged"
		docs = append(docs, d)
	}
	extras := make([]string, 0)
	for id := range corpus {
		if _, ok := required[id]; !ok {
			extras = append(extras, id)
		}
	}
	sort.Strings(extras)
	for _, id := range extras {
		if len(docs) >= opts.TargetDocuments {
			break
		}
		d := corpus[id]
		d.ContentSHA256 = contentHash(d.Text)
		d.JudgmentStatus = "unjudged"
		docs = append(docs, d)
	}
	if len(docs) < opts.MinDocuments || len(docs) > opts.MaxDocuments {
		return PreparedDataset{}, fmt.Errorf("document count %d outside %d..%d", len(docs), opts.MinDocuments, opts.MaxDocuments)
	}
	docIDs := make([]string, len(docs))
	for i, d := range docs {
		docIDs[i] = d.SourceDocumentID
	}
	qb, _ := json.Marshal(qrels)
	cb, _ := json.Marshal(docs)
	cfgb, _ := json.Marshal(opts)
	m := DatasetManifest{SchemaVersion: ManifestSchemaVersion, Dataset: opts.Dataset, Revision: opts.Revision, Language: opts.Language, Split: opts.Split, License: opts.License, SourceURL: opts.SourceURL, SelectionSeed: opts.Seed, QueryLimit: opts.QueryLimit, TargetDocuments: opts.TargetDocuments, MinDocuments: opts.MinDocuments, MaxDocuments: opts.MaxDocuments, QueryIDs: make([]string, len(outQueries)), DocumentIDs: docIDs, QrelCount: len(qrels), QrelsSHA256: hashBytes(qb), CorpusSHA256: hashBytes(cb), ConfigSHA256: hashBytes(cfgb)}
	for i, q := range outQueries {
		m.QueryIDs[i] = q.ID
	}
	return PreparedDataset{
		Manifest: m, Queries: outQueries, Documents: docs, Qrels: qrels,
		PrepareStats: PhaseStats{ElapsedMS: time.Since(started).Milliseconds(), DocumentCount: len(docs), QueryCount: len(outQueries)},
	}, nil
}

func WriteDataset(target string, ds PreparedDataset) error {
	if err := ds.Manifest.Validate(); err != nil {
		return err
	}
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".miracl-mini-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	files := map[string]any{"manifest.json": ds.Manifest, "queries.json": ds.Queries, "documents.json": ds.Documents, "qrels.json": ds.Qrels, "prepare_stats.json": ds.PrepareStats}
	for name, v := range files {
		b, e := json.MarshalIndent(v, "", "  ")
		if e != nil {
			return e
		}
		if e = os.WriteFile(filepath.Join(tmp, name), append(b, '\n'), 0o644); e != nil {
			return e
		}
	}
	backup := target + ".previous"
	hadTarget := false
	if _, err := os.Stat(target); err == nil {
		hadTarget = true
		if err := os.RemoveAll(backup); err != nil {
			return err
		}
		if err := os.Rename(target, backup); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		if hadTarget {
			_ = os.Rename(backup, target)
		}
		return err
	}
	if hadTarget {
		_ = os.RemoveAll(backup)
	}
	return nil
}

// ReadMIRACL loads the standard MIRACL TSV topics/qrels and sharded JSONL
// corpus. It streams corpus shards so the complete corpus is never held in
// memory; callers can then apply PrepareDataset's deterministic subset rule.
func ReadMIRACL(topicsPath, qrelsPath string, corpusPaths []string) (DatasetInput, error) {
	var in DatasetInput
	if err := readTopics(topicsPath, &in.Queries); err != nil {
		return in, err
	}
	if err := readQrels(qrelsPath, &in.Qrels); err != nil {
		return in, err
	}
	for _, path := range corpusPaths {
		if err := readCorpusShard(path, &in.Corpus); err != nil {
			return in, err
		}
	}
	return in, nil
}

// PrepareMIRACLFiles is the bounded-memory path used by the CLI. MIRACL's
// Chinese corpus has millions of passages; only judged documents and the
// deterministic smallest-hash unjudged documents are retained.
func PrepareMIRACLFiles(topicsPath, qrelsPath string, corpusPaths []string, opts PrepareOptions) (PreparedDataset, error) {
	var queries []BenchmarkQuery
	if err := readTopics(topicsPath, &queries); err != nil {
		return PreparedDataset{}, err
	}
	var qrels []Qrel
	if err := readQrels(qrelsPath, &qrels); err != nil {
		return PreparedDataset{}, err
	}
	selected, err := selectQueryIDs(queries, qrels, opts)
	if err != nil {
		return PreparedDataset{}, err
	}
	required := map[string]struct{}{}
	for _, q := range qrels {
		if _, ok := selected[q.QueryID]; ok {
			required[q.SourceDocumentID] = struct{}{}
		}
	}
	extraLimit := opts.TargetDocuments
	if extraLimit <= 0 {
		extraLimit = 800
	}
	extraLimit -= len(required)
	if extraLimit < 0 {
		extraLimit = 0
	}
	h := &documentMinHeap{}
	for _, path := range corpusPaths {
		if err := scanCorpusShard(path, func(d BenchmarkDocument) error {
			if _, ok := required[d.SourceDocumentID]; ok {
				return nil
			}
			if extraLimit == 0 {
				return nil
			}
			item := heapItem{key: hashBytes([]byte(strconv.FormatInt(opts.Seed, 10) + "/" + d.SourceDocumentID)), doc: d}
			if h.Len() < extraLimit {
				heap.Push(h, item)
			} else if item.key < (*h)[0].key {
				heap.Pop(h)
				heap.Push(h, item)
			}
			return nil
		}); err != nil {
			return PreparedDataset{}, err
		}
	}
	corpus := make([]BenchmarkDocument, 0, len(required)+h.Len())
	for _, path := range corpusPaths {
		if err := scanCorpusShard(path, func(d BenchmarkDocument) error {
			if _, ok := required[d.SourceDocumentID]; ok {
				corpus = append(corpus, d)
			}
			return nil
		}); err != nil {
			return PreparedDataset{}, err
		}
	}
	for h.Len() > 0 {
		corpus = append(corpus, heap.Pop(h).(heapItem).doc)
	}
	datasetInput := DatasetInput{Queries: queries, Qrels: qrels, Corpus: corpus}
	return PrepareDataset(datasetInput, opts)
}

func readTopics(path string, out *[]BenchmarkQuery) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		p := strings.SplitN(sc.Text(), "\t", 2)
		if len(p) != 2 {
			continue
		}
		*out = append(*out, BenchmarkQuery{ID: strings.TrimSpace(p[0]), Text: strings.TrimSpace(p[1])})
	}
	return sc.Err()
}
func readQrels(path string, out *[]Qrel) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		p := strings.Fields(sc.Text())
		if len(p) < 4 {
			continue
		}
		rel, e := strconv.Atoi(p[3])
		if e != nil {
			return fmt.Errorf("parse qrel relevance: %w", e)
		}
		*out = append(*out, Qrel{QueryID: p[0], SourceDocumentID: p[2], Relevance: rel})
	}
	return sc.Err()
}
func readCorpusShard(path string, out *[]BenchmarkDocument) error {
	return scanCorpusShard(path, func(d BenchmarkDocument) error { *out = append(*out, d); return nil })
}
func scanCorpusShard(path string, fn func(BenchmarkDocument) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	var gz *gzip.Reader
	if strings.HasSuffix(path, ".gz") {
		gz, err = gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("open corpus gzip: %w", err)
		}
		defer gz.Close()
		r = gz
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		var row struct {
			ID    string `json:"_id"`
			ID2   string `json:"id"`
			ID3   string `json:"docid"`
			Title string `json:"title"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return fmt.Errorf("parse corpus %s: %w", path, err)
		}
		id := row.ID
		if id == "" {
			id = row.ID2
		}
		if id == "" {
			id = row.ID3
		}
		if id != "" {
			if err := fn(BenchmarkDocument{SourceDocumentID: id, Title: row.Title, Text: row.Text}); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

func selectQueryIDs(queries []BenchmarkQuery, qrels []Qrel, opts PrepareOptions) (map[string]struct{}, error) {
	querySet := map[string]struct{}{}
	for _, q := range queries {
		querySet[q.ID] = struct{}{}
	}
	positive := map[string]bool{}
	for _, q := range qrels {
		if _, ok := querySet[q.QueryID]; !ok {
			return nil, fmt.Errorf("qrel references missing query %s", q.QueryID)
		}
		if q.Relevance > 0 {
			positive[q.QueryID] = true
		}
	}
	type c struct{ id, key string }
	candidates := []c{}
	for _, q := range queries {
		if positive[q.ID] {
			candidates = append(candidates, c{q.ID, hashBytes([]byte(strconv.FormatInt(opts.Seed, 10) + "/" + q.ID))})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].key == candidates[j].key {
			return candidates[i].id < candidates[j].id
		}
		return candidates[i].key < candidates[j].key
	})
	limit := opts.QueryLimit
	if limit <= 0 {
		limit = 50
	}
	if len(candidates) < limit {
		return nil, fmt.Errorf("only %d queries with positive qrels", len(candidates))
	}
	out := map[string]struct{}{}
	for _, c := range candidates[:limit] {
		out[c.id] = struct{}{}
	}
	return out, nil
}

type heapItem struct {
	key string
	doc BenchmarkDocument
}
type documentMinHeap []heapItem

func (h documentMinHeap) Len() int           { return len(h) }
func (h documentMinHeap) Less(i, j int) bool { return h[i].key > h[j].key }
func (h documentMinHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *documentMinHeap) Push(x any)        { *h = append(*h, x.(heapItem)) }
func (h *documentMinHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}
