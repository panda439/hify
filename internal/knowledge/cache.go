package knowledge

import "sync"

// chunkCache holds each knowledge base's full chunk set (with embeddings)
// in memory, avoiding a full MySQL scan on every RAG-augmented chat turn —
// see "已知性能风险" #1 in the plan. A single in-process map is sufficient
// at Hify's current single-instance deployment scale; this is deliberately
// not Redis-backed (see CLAUDE.md/plan for the upgrade path if that ever
// changes).
type chunkCache struct {
	mu      sync.RWMutex
	entries map[string][]Chunk
}

func newChunkCache() *chunkCache {
	return &chunkCache{entries: make(map[string][]Chunk)}
}

func (c *chunkCache) get(knowledgeBaseID string) ([]Chunk, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	chunks, ok := c.entries[knowledgeBaseID]
	return chunks, ok
}

func (c *chunkCache) set(knowledgeBaseID string, chunks []Chunk) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[knowledgeBaseID] = chunks
}

// invalidate is called whenever a knowledge base's chunk set changes
// (document finishes processing, document deleted) so the next retrieval
// rebuilds from MySQL instead of serving stale vectors.
func (c *chunkCache) invalidate(knowledgeBaseID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, knowledgeBaseID)
}
