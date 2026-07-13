package provider

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"

	"hify/internal/db/gen"
)

const mysqlDuplicateEntry = 1062

// Repository is constructed via NewRepository in wire.go.
type Repository struct {
	queries *gen.Queries
}

func (r *Repository) createProvider(ctx context.Context, p Provider, encryptedKey []byte) error {
	headersJSON, err := json.Marshal(p.ExtraHeaders)
	if err != nil {
		return fmt.Errorf("provider: marshal extra_headers: %w", err)
	}
	configJSON, err := json.Marshal(p.ExtraConfig)
	if err != nil {
		return fmt.Errorf("provider: marshal extra_config: %w", err)
	}

	err = r.queries.CreateProvider(ctx, gen.CreateProviderParams{
		ID:              p.ID,
		Name:            p.Name,
		AdapterType:     p.AdapterType,
		BaseUrl:         p.BaseURL,
		AuthType:        p.AuthType,
		ApiKeyEncrypted: bytesToNullString(encryptedKey),
		ExtraHeaders:    headersJSON,
		ExtraConfig:     configJSON,
		CreatedBy:       p.CreatedBy,
	})
	if err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == mysqlDuplicateEntry {
			return ErrNameTaken
		}
		return fmt.Errorf("provider: create: %w", err)
	}
	return nil
}

func (r *Repository) getProvider(ctx context.Context, id string) (Provider, error) {
	row, err := r.queries.GetProviderByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Provider{}, ErrNotFound
		}
		return Provider{}, fmt.Errorf("provider: get by id: %w", err)
	}
	return toDomainProvider(row)
}

// getEncryptedAPIKey is the one place ciphertext leaves the storage layer —
// used only by registry.go to decrypt before constructing a Client. The
// public Provider type never carries it (mirrors how internal/user keeps
// the password hash out of its domain type).
func (r *Repository) getEncryptedAPIKey(ctx context.Context, id string) ([]byte, error) {
	row, err := r.queries.GetProviderByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("provider: get encrypted key: %w", err)
	}
	if !row.ApiKeyEncrypted.Valid {
		return nil, nil
	}
	return []byte(row.ApiKeyEncrypted.String), nil
}

func (r *Repository) listProviders(ctx context.Context, limit, offset int) ([]Provider, error) {
	rows, err := r.queries.ListProviders(ctx, gen.ListProvidersParams{
		Limit:  int32(limit),
		Offset: int32(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("provider: list: %w", err)
	}
	out := make([]Provider, 0, len(rows))
	for _, row := range rows {
		p, err := toDomainProvider(row)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (r *Repository) countProviders(ctx context.Context) (int, error) {
	n, err := r.queries.CountProviders(ctx)
	if err != nil {
		return 0, fmt.Errorf("provider: count: %w", err)
	}
	return int(n), nil
}

func (r *Repository) updateProvider(ctx context.Context, p Provider) error {
	headersJSON, err := json.Marshal(p.ExtraHeaders)
	if err != nil {
		return fmt.Errorf("provider: marshal extra_headers: %w", err)
	}
	configJSON, err := json.Marshal(p.ExtraConfig)
	if err != nil {
		return fmt.Errorf("provider: marshal extra_config: %w", err)
	}

	err = r.queries.UpdateProvider(ctx, gen.UpdateProviderParams{
		Name:         p.Name,
		BaseUrl:      p.BaseURL,
		AuthType:     p.AuthType,
		ExtraHeaders: headersJSON,
		ExtraConfig:  configJSON,
		IsActive:     p.IsActive,
		ID:           p.ID,
	})
	if err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == mysqlDuplicateEntry {
			return ErrNameTaken
		}
		return fmt.Errorf("provider: update: %w", err)
	}
	return nil
}

func (r *Repository) updateAPIKey(ctx context.Context, id string, encryptedKey []byte) error {
	if err := r.queries.UpdateProviderAPIKey(ctx, gen.UpdateProviderAPIKeyParams{
		ApiKeyEncrypted: bytesToNullString(encryptedKey),
		ID:              id,
	}); err != nil {
		return fmt.Errorf("provider: update api key: %w", err)
	}
	return nil
}

func (r *Repository) updateTestResult(ctx context.Context, id, status, testErr string) error {
	if err := r.queries.UpdateProviderTestResult(ctx, gen.UpdateProviderTestResultParams{
		LastTestedAt:   sql.NullTime{Time: time.Now(), Valid: true},
		LastTestStatus: status,
		LastTestError:  sql.NullString{String: testErr, Valid: testErr != ""},
		ID:             id,
	}); err != nil {
		return fmt.Errorf("provider: update test result: %w", err)
	}
	return nil
}

func (r *Repository) createModel(ctx context.Context, m Model) error {
	err := r.queries.CreateProviderModel(ctx, gen.CreateProviderModelParams{
		ID:                 m.ID,
		ProviderID:         m.ProviderID,
		ModelName:          m.ModelName,
		Capability:         m.Capability,
		ContextWindow:      intPtrToNullInt32(m.ContextWindow),
		MaxOutputTokens:    intPtrToNullInt32(m.MaxOutputTokens),
		EmbeddingDimension: intPtrToNullInt32(m.EmbeddingDimension),
		IsDefault:          m.IsDefault,
	})
	if err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == mysqlDuplicateEntry {
			return ErrModelNameTaken
		}
		return fmt.Errorf("provider: create model: %w", err)
	}
	return nil
}

func (r *Repository) getModel(ctx context.Context, id string) (Model, error) {
	row, err := r.queries.GetProviderModelByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Model{}, ErrModelNotFound
		}
		return Model{}, fmt.Errorf("provider: get model: %w", err)
	}
	return toDomainModel(row), nil
}

func (r *Repository) listModelsByProvider(ctx context.Context, providerID string) ([]Model, error) {
	rows, err := r.queries.ListProviderModelsByProvider(ctx, providerID)
	if err != nil {
		return nil, fmt.Errorf("provider: list models: %w", err)
	}
	out := make([]Model, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainModel(row))
	}
	return out, nil
}

func (r *Repository) listActiveModelsByCapability(ctx context.Context, capability string) ([]Model, error) {
	rows, err := r.queries.ListActiveModelsByCapability(ctx, capability)
	if err != nil {
		return nil, fmt.Errorf("provider: list models by capability: %w", err)
	}
	out := make([]Model, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainModel(row))
	}
	return out, nil
}

func (r *Repository) updateModel(ctx context.Context, m Model) error {
	if err := r.queries.UpdateProviderModel(ctx, gen.UpdateProviderModelParams{
		ContextWindow:      intPtrToNullInt32(m.ContextWindow),
		MaxOutputTokens:    intPtrToNullInt32(m.MaxOutputTokens),
		EmbeddingDimension: intPtrToNullInt32(m.EmbeddingDimension),
		IsDefault:          m.IsDefault,
		IsActive:           m.IsActive,
		ID:                 m.ID,
	}); err != nil {
		return fmt.Errorf("provider: update model: %w", err)
	}
	return nil
}

func toDomainProvider(row gen.ModelProvider) (Provider, error) {
	var headers map[string]string
	if len(row.ExtraHeaders) > 0 {
		if err := json.Unmarshal(row.ExtraHeaders, &headers); err != nil {
			return Provider{}, fmt.Errorf("provider: unmarshal extra_headers: %w", err)
		}
	}
	var cfg ExtraConfig
	if len(row.ExtraConfig) > 0 {
		if err := json.Unmarshal(row.ExtraConfig, &cfg); err != nil {
			return Provider{}, fmt.Errorf("provider: unmarshal extra_config: %w", err)
		}
	}
	var lastTestedAt *time.Time
	if row.LastTestedAt.Valid {
		t := row.LastTestedAt.Time
		lastTestedAt = &t
	}
	return Provider{
		ID:             row.ID,
		Name:           row.Name,
		AdapterType:    row.AdapterType,
		BaseURL:        row.BaseUrl,
		AuthType:       row.AuthType,
		HasAPIKey:      row.ApiKeyEncrypted.Valid,
		ExtraHeaders:   headers,
		ExtraConfig:    cfg,
		LastTestedAt:   lastTestedAt,
		LastTestStatus: row.LastTestStatus,
		LastTestError:  row.LastTestError.String,
		IsActive:       row.IsActive,
		CreatedBy:      row.CreatedBy,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}, nil
}

func toDomainModel(row gen.ProviderModel) Model {
	return Model{
		ID:                 row.ID,
		ProviderID:         row.ProviderID,
		ModelName:          row.ModelName,
		Capability:         row.Capability,
		ContextWindow:      nullInt32ToPtr(row.ContextWindow),
		MaxOutputTokens:    nullInt32ToPtr(row.MaxOutputTokens),
		EmbeddingDimension: nullInt32ToPtr(row.EmbeddingDimension),
		IsDefault:          row.IsDefault,
		IsActive:           row.IsActive,
		CreatedAt:          row.CreatedAt,
		UpdatedAt:          row.UpdatedAt,
	}
}

func bytesToNullString(b []byte) sql.NullString {
	if len(b) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: string(b), Valid: true}
}

func intPtrToNullInt32(p *int) sql.NullInt32 {
	if p == nil {
		return sql.NullInt32{}
	}
	return sql.NullInt32{Int32: int32(*p), Valid: true}
}

func nullInt32ToPtr(n sql.NullInt32) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int32)
	return &v
}
