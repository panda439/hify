package knowledge

import "math"

// cosineSimilarity is the whole "vector search" this module does — no
// index, no ANN, just a dot product over two float32 slices. See the
// plan's Context section for why: at Hify's current scale (thousands to
// tens of thousands of chunks per knowledge base) this is fast enough that
// a dedicated vector database would be solving a problem that doesn't
// exist yet.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
