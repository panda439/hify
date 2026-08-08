package retrieval

import (
	"reflect"
	"strings"
	"testing"
)

// TestGateReportTypesCarryNoForbiddenFields is the automated privacy check
// Codex's first-round Phase 6 review asked for (待修复项 1): an earlier
// version of GateHit carried a Score field, which is not on the task's
// allow-list ("case 名、chunk/document ID、rank、NeighborOf、计数与聚合指
// 标"). Checking this by reading the struct definitions is exactly the
// "只靠人工阅读结构体" the review flagged as insufficient — this instead
// walks GateReport's full type graph via reflection and fails if any
// field's Go name or json tag matches a forbidden term, so a future change
// that reintroduces Score (or adds Content/Query/Embedding/a fingerprint
// field) anywhere in this report's shape fails a fast, deterministic unit
// test instead of shipping unnoticed.
func TestGateReportTypesCarryNoForbiddenFields(t *testing.T) {
	forbidden := []string{"content", "score", "query", "embedding", "fingerprint", "vector"}
	visited := map[reflect.Type]bool{}
	walkTypeForForbiddenFields(t, reflect.TypeOf(GateReport{}), forbidden, visited)
}

func walkTypeForForbiddenFields(t *testing.T, typ reflect.Type, forbidden []string, visited map[reflect.Type]bool) {
	t.Helper()
	for typ.Kind() == reflect.Ptr || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array || typ.Kind() == reflect.Map {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	if visited[typ] {
		return // avoid infinite recursion on any future self-referential type
	}
	visited[typ] = true

	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		name := strings.ToLower(f.Name)
		if tag != "" && tag != "-" {
			name = strings.ToLower(tag)
		}
		for _, word := range forbidden {
			if name == word {
				t.Fatalf("%s.%s (json tag %q) is a forbidden field — task requirement 5 only allows case name, chunk/document ID, rank, NeighborOf, counts and aggregate metrics; %q must never appear in this report's type graph",
					typ.Name(), f.Name, f.Tag.Get("json"), word)
			}
		}
		walkTypeForForbiddenFields(t, f.Type, forbidden, visited)
	}
}
