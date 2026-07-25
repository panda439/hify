package knowledge

import "strings"

// chunkText splits text into overlapping chunks of at most size runes,
// stepping by (size - overlap) each time. Operates on runes, not bytes —
// Hify's content is often Chinese, where byte-slicing would cut a
// multi-byte UTF-8 character in half.
func chunkText(text string, size, overlap int) []string {
	if size <= 0 {
		size = defaultChunkSize
	}
	if overlap < 0 || overlap >= size {
		// A misconfigured overlap must not turn this into an infinite
		// loop (step would be <= 0) — fall back to no overlap.
		overlap = 0
	}

	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}

	step := size - overlap
	var chunks []string
	for start := 0; start < len(runes); start += step {
		end := start + size
		if end > len(runes) {
			end = len(runes)
		}
		if chunk := strings.TrimSpace(string(runes[start:end])); chunk != "" {
			chunks = append(chunks, chunk)
		}
		if end == len(runes) {
			break
		}
	}
	return chunks
}

// batchStrings splits items into groups of at most size, preserving
// order — ProcessDocument uses it to keep each provider.Client.Embed call
// bounded by embedBatchSize regardless of how many pieces chunkText
// produced (up to maxChunksPerDocument).
func batchStrings(items []string, size int) [][]string {
	if size <= 0 || len(items) == 0 {
		return nil
	}
	batches := make([][]string, 0, (len(items)+size-1)/size)
	for start := 0; start < len(items); start += size {
		end := start + size
		if end > len(items) {
			end = len(items)
		}
		batches = append(batches, items[start:end])
	}
	return batches
}
