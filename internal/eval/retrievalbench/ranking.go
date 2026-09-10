package retrievalbench

// FoldChunkRanking removes duplicate documents while preserving the first chunk's order.
func FoldChunkRanking(hits []ChunkHit, k int) []string {
	seen := make(map[string]struct{}, len(hits))
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		if hit.DocumentID == "" {
			continue
		}
		if _, ok := seen[hit.DocumentID]; ok {
			continue
		}
		seen[hit.DocumentID] = struct{}{}
		out = append(out, hit.DocumentID)
		if k > 0 && len(out) == k {
			break
		}
	}
	return out
}
