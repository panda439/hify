package knowledge

import (
	"database/sql"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"

	"hify/internal/db/gen"
	"hify/internal/db/pggen"
	"hify/internal/platform/httperr"
	"hify/internal/provider"
	"hify/internal/server/middleware"
)

// NewRepository takes both pools: db (MySQL) for knowledge_bases/documents,
// pgdb (PostgreSQL+pgvector) for chunks — see the Repository doc comment.
func NewRepository(db, pgdb *sql.DB) *Repository {
	return &Repository{db: db, queries: gen.New(db), pgdb: pgdb, pgQueries: pggen.New(pgdb)}
}

// NewService needs the asynq client to enqueue document-processing jobs
// and a local directory to store uploaded files under (see the "本地磁盘存
// 储是当前阶段的已知限制" note in the plan — not multi-instance safe, fine
// at Hify's current single-instance deployment scale). rerankEnabled/
// rerankModelID/rerankTimeout are 001-rag-query-rerank's rerank config
// (data-model.md §3).
func NewService(repo *Repository, providerSvc provider.Service, asynqClient *asynq.Client, storageDir string, rerankEnabled bool, rerankModelID string, rerankTimeout time.Duration, metadataFilterEnabled bool) Service {
	s := &service{
		repo:        repo,
		providerSvc: providerSvc,
		asynqClient: asynqClient,
		storageDir:  storageDir,
		// See the service struct's findNeighborBatch doc comment for why
		// this is a method value instead of expandWithNeighborWindow
		// calling repo.findPublishedNeighborChunksBatch directly.
		findNeighborBatch: repo.findPublishedNeighborChunksBatch,
		rerankEnabled:     rerankEnabled,
		rerankModelID:     rerankModelID,
		rerankTimeout:     rerankTimeout,

		metadataFilterEnabled: metadataFilterEnabled,
	}
	// rerankScoreFn defaults to s.resolveRerankScores (see the service
	// struct's doc comment for why this is a replaceable method-value
	// field, same pattern as findNeighborBatch above) — self-referential
	// construction (s must exist before its own method can be taken as a
	// value), unlike findNeighborBatch which is bound to the already-
	// constructed repo parameter instead.
	s.rerankScoreFn = s.resolveRerankScores
	return s
}

func NewHandler(svc Service) *Handler {
	return &Handler{service: svc}
}

// RegisterRoutes wires knowledge bases onto "/api/v1/knowledge-bases" —
// every authenticated user can create/list/view (mirrors agent's
// all-visible model); ownership enforcement for edits/uploads/deletes
// happens inside service.go.
func RegisterRoutes(v1 *gin.RouterGroup, h *Handler, jwtSecret string) {
	kbs := v1.Group("/knowledge-bases")
	kbs.Use(middleware.RequireAuth(jwtSecret))

	kbs.POST("", httperr.Wrap(h.Create))
	kbs.GET("", httperr.Wrap(h.List))
	kbs.GET("/:id", httperr.Wrap(h.Get))
	kbs.PUT("/:id", httperr.Wrap(h.Update))
	// 003-retrieval-playground：试检索。语义是查询而非创建资源，用 POST 只是
	// 因为请求体里有文档 ID 数组和问题原文，且问题原文不应进 URL（会被网关/
	// 代理的访问日志记下来）——与 002 不把过滤取值写进应用日志是同一个口径。
	kbs.POST("/:id/retrieve", httperr.Wrap(h.Retrieve))
	kbs.POST("/:id/documents", httperr.Wrap(h.UploadDocument))
	kbs.GET("/:id/documents", httperr.Wrap(h.ListDocuments))
	kbs.GET("/:id/documents/:docId", httperr.Wrap(h.GetDocument))
	kbs.DELETE("/:id/documents/:docId", httperr.Wrap(h.DeleteDocument))
	kbs.POST("/:id/documents/:docId/retry", httperr.Wrap(h.RetryDocument))
}
