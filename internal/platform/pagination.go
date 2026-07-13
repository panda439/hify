package platform

// MaxPageLimit is the hard server-side cap on any LIMIT, regardless of what
// a client requests — see "分页查询注意事项" in CLAUDE.md.
const MaxPageLimit = 200

const defaultPageLimit = 20

// ClampLimit enforces MaxPageLimit and a sane default; callers must not
// trust a client-supplied limit directly.
func ClampLimit(limit int) int {
	if limit <= 0 {
		return defaultPageLimit
	}
	if limit > MaxPageLimit {
		return MaxPageLimit
	}
	return limit
}

// CursorPage is the response shape for keyset-paginated large tables
// (messages/chunks/workflow_run_steps). NextCursor is empty when there is no
// further page; it deliberately carries no total count (see CLAUDE.md).
type CursorPage[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// OffsetPage is the response shape for small admin-list tables
// (providers/users/agents/workflows) where OFFSET/LIMIT is acceptable.
type OffsetPage[T any] struct {
	Items    []T `json:"items"`
	Total    int `json:"total"`
	Page     int `json:"page"`
	PageSize int `json:"page_size"`
}
