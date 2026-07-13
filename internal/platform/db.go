package platform

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// DefaultQueryTimeout bounds every query issued through this pool so a slow
// query cannot hold a connection indefinitely and starve unrelated requests.
const DefaultQueryTimeout = 5 * time.Second

// NewMySQLPool opens a connection pool sized for the current single-instance
// deployment. MaxOpenConns/MaxIdleConns must stay bounded: an unbounded pool
// lets one slow query exhaust connections and cascade into unrelated request
// latency (see "已知性能风险与应对" #2 in the project plan).
func NewMySQLPool(dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("platform: open mysql: %w", err)
	}

	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(2 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), DefaultQueryTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("platform: ping mysql: %w", err)
	}

	return db, nil
}
