package knowledge

import (
	"database/sql"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"

	"hify/internal/db/gen"
	"hify/internal/platform/httperr"
	"hify/internal/provider"
	"hify/internal/server/middleware"
)

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db, queries: gen.New(db)}
}

// NewService needs the asynq client to enqueue document-processing jobs
// and a local directory to store uploaded files under (see the "本地磁盘存
// 储是当前阶段的已知限制" note in the plan — not multi-instance safe, fine
// at Hify's current single-instance deployment scale).
func NewService(repo *Repository, providerSvc provider.Service, asynqClient *asynq.Client, storageDir string) Service {
	return &service{
		repo:        repo,
		providerSvc: providerSvc,
		asynqClient: asynqClient,
		storageDir:  storageDir,
		cache:       newChunkCache(),
	}
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
	kbs.POST("/:id/documents", httperr.Wrap(h.UploadDocument))
	kbs.GET("/:id/documents", httperr.Wrap(h.ListDocuments))
	kbs.GET("/:id/documents/:docId", httperr.Wrap(h.GetDocument))
	kbs.DELETE("/:id/documents/:docId", httperr.Wrap(h.DeleteDocument))
}
