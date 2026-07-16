package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	pgvector "github.com/pgvector/pgvector-go"

	"hify/internal/config"
	"hify/internal/platform"
)

// backfillBatchSize keeps each keyset page (and its PG transaction) small
// enough to stay well inside platform.DefaultQueryTimeout on both sides.
const backfillBatchSize = 1000

// runBackfillChunks copies pre-migration chunks from MySQL (embedding as a
// JSON float array) into the pgvector PostgreSQL chunks table. It is a
// one-off transitional command, safe to delete once every deployment has
// crossed the migration:
//
//   - It applies the PG migrations itself first, so it can (and must) run
//     BEFORE `hify migrate up` — which applies MySQL 000007 and drops the
//     source table. Running in the wrong order is recoverable (chunks are
//     derived data; re-upload documents), but loses the cheap path.
//   - Idempotent by construction: inserts use ON CONFLICT (id) DO NOTHING,
//     and a missing MySQL chunks table (fresh install, or re-run after the
//     drop) is treated as "nothing to backfill", not an error.
//
// Both SQL statements are hand-written rather than sqlc-generated on
// purpose: the MySQL chunks table no longer exists in sqlc's schema view
// (000007 dropped it), and the PG insert needs the explicit created_at and
// ON CONFLICT clause the regular CreateChunk query doesn't have.
func runBackfillChunks(cfg config.Config, logger *slog.Logger) error {
	ctx := context.Background()

	if err := migratePostgres(cfg, "up"); err != nil {
		return fmt.Errorf("backfill: ensure postgres schema: %w", err)
	}

	db, err := platform.NewMySQLPool(cfg.MySQLDSN)
	if err != nil {
		return fmt.Errorf("backfill: connect mysql: %w", err)
	}
	defer db.Close()

	pgdb, err := platform.NewPostgresPool(cfg.PostgresDSN)
	if err != nil {
		return fmt.Errorf("backfill: connect postgres: %w", err)
	}
	defer pgdb.Close()

	exists, err := mysqlChunksTableExists(ctx, db)
	if err != nil {
		return err
	}
	if !exists {
		logger.Info("backfill-chunks: mysql chunks table does not exist, nothing to backfill")
		return nil
	}

	var mysqlCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM chunks").Scan(&mysqlCount); err != nil {
		return fmt.Errorf("backfill: count mysql chunks: %w", err)
	}

	copied := 0
	lastID := ""
	for {
		batch, err := readMySQLChunkBatch(ctx, db, lastID)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		if err := insertPGChunkBatch(ctx, pgdb, batch); err != nil {
			return err
		}
		copied += len(batch)
		lastID = batch[len(batch)-1].id
		logger.Info("backfill-chunks: batch copied", "batch", len(batch), "total", copied)
	}

	var pgCount int
	if err := pgdb.QueryRowContext(ctx, "SELECT COUNT(*) FROM chunks").Scan(&pgCount); err != nil {
		return fmt.Errorf("backfill: count postgres chunks: %w", err)
	}

	logger.Info("backfill-chunks: done",
		"mysql_chunks", mysqlCount,
		"postgres_chunks", pgCount,
		"copied_this_run", copied,
	)
	if pgCount < mysqlCount {
		return fmt.Errorf("backfill: postgres has %d chunks but mysql has %d — counts must not diverge", pgCount, mysqlCount)
	}
	return nil
}

func mysqlChunksTableExists(ctx context.Context, db *sql.DB) (bool, error) {
	var name string
	err := db.QueryRowContext(ctx,
		"SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'chunks'",
	).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("backfill: check mysql chunks table: %w", err)
	}
	return true, nil
}

type backfillChunk struct {
	id                 string
	knowledgeBaseID    string
	documentID         string
	chunkIndex         int32
	content            string
	contentLength      int32
	embeddingJSON      []byte
	embeddingDimension int32
	createdAt          sql.NullTime
}

// readMySQLChunkBatch keyset-paginates by primary key — chunk ids are
// UUIDv7 strings, so `id > lastID ORDER BY id` walks the table in stable
// insertion-ish order without OFFSET's quadratic re-scanning.
func readMySQLChunkBatch(ctx context.Context, db *sql.DB, lastID string) ([]backfillChunk, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, knowledge_base_id, document_id, chunk_index, content,
		        content_length, embedding, embedding_dimension, created_at
		 FROM chunks WHERE id > ? ORDER BY id LIMIT ?`,
		lastID, backfillBatchSize,
	)
	if err != nil {
		return nil, fmt.Errorf("backfill: read mysql chunks: %w", err)
	}
	defer rows.Close()

	var out []backfillChunk
	for rows.Next() {
		var c backfillChunk
		if err := rows.Scan(
			&c.id, &c.knowledgeBaseID, &c.documentID, &c.chunkIndex, &c.content,
			&c.contentLength, &c.embeddingJSON, &c.embeddingDimension, &c.createdAt,
		); err != nil {
			return nil, fmt.Errorf("backfill: scan mysql chunk: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("backfill: iterate mysql chunks: %w", err)
	}
	return out, nil
}

func insertPGChunkBatch(ctx context.Context, pgdb *sql.DB, batch []backfillChunk) error {
	return platform.WithTx(ctx, pgdb, func(tx *sql.Tx) error {
		for _, c := range batch {
			var embedding []float32
			if err := json.Unmarshal(c.embeddingJSON, &embedding); err != nil {
				return fmt.Errorf("backfill: unmarshal embedding for chunk %s: %w", c.id, err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO chunks (id, knowledge_base_id, document_id, chunk_index,
				                     content, content_length, embedding,
				                     embedding_dimension, created_at)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				 ON CONFLICT (id) DO NOTHING`,
				c.id, c.knowledgeBaseID, c.documentID, c.chunkIndex,
				c.content, c.contentLength, pgvector.NewVector(embedding),
				c.embeddingDimension, c.createdAt.Time,
			); err != nil {
				return fmt.Errorf("backfill: insert chunk %s: %w", c.id, err)
			}
		}
		return nil
	})
}
