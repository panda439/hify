package knowledge

import "hify/internal/platform/apperr"

var (
	ErrNotFound              = apperr.NotFound("knowledge.not_found", "知识库不存在")
	ErrForbidden             = apperr.Forbidden("knowledge.forbidden", "只有创建者本人或管理员可以修改该知识库")
	ErrDocumentNotFound      = apperr.NotFound("knowledge.document_not_found", "文档不存在")
	ErrInvalidEmbeddingModel = apperr.InvalidInput("knowledge.invalid_embedding_model", "所选模型不可用，请选择一个已启用的向量模型")
	ErrUnsupportedFileType   = apperr.InvalidInput("knowledge.unsupported_file_type", "不支持的文件类型，仅支持 txt/md/pdf")
	ErrFileTooLarge          = apperr.InvalidInput("knowledge.file_too_large", "文件过大（超过 10MB），请拆分后重新上传")
	ErrInvalidRequest        = apperr.InvalidInput("knowledge.invalid_request", "请求参数不正确")
)
