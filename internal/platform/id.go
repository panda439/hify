package platform

import (
	"fmt"

	"github.com/google/uuid"
)

// NewID generates a UUIDv7 primary key: time-ordered so InnoDB's clustered
// primary-key index doesn't fragment under random-order inserts the way a
// UUIDv4 would (see "数据库层性能规范" in CLAUDE.md).
func NewID() string {
	id, err := uuid.NewV7()
	if err != nil {
		// The entropy source failing is not a recoverable business-logic
		// condition; there is no sane fallback that preserves the ordering
		// property callers rely on.
		panic(fmt.Errorf("platform: generate uuidv7: %w", err))
	}
	return id.String()
}
