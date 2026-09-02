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

	// Document-processing failures (see service.go's ProcessDocument /
	// failDocument) — Message stays fixed/Chinese, any dynamic detail
	// (counts, file paths) goes to the log instead, same "Message can't
	// carry dynamic content" rule workflow's errors.go documents.
	ErrEmptyContent           = apperr.InvalidInput("knowledge.empty_content", "文档内容为空或无法提取到文本")
	ErrEmbeddingCountMismatch = apperr.InvalidInput("knowledge.embedding_count_mismatch", "向量生成数量与分块数量不一致，请重试")
	ErrTooManyChunks          = apperr.InvalidInput("knowledge.too_many_chunks", "文档分块数量超出单文档上限，请拆分文件后重新上传")

	// ErrEmbeddingDimensionMismatch guards validateEmbedBatch — a later
	// batch reporting a different dimension than the first batch
	// established, a non-positive dimension, or a vector whose actual
	// length disagrees with the batch's own declared dimension.
	ErrEmbeddingDimensionMismatch = apperr.InvalidInput("knowledge.embedding_dimension_mismatch", "向量生成维度不一致，请重试或联系管理员检查供应商配置")

	// ErrDocumentNotRetryable guards RetryDocument's CAS — only
	// pending/failed documents can be retried, matching the legal state
	// transitions in the Document doc comment.
	// 002-metadata-filter：检索范围过滤的三个入参错误。它们都发生在任何
	// 数据库调用之前（service.Retrieve 的入口校验），因此不经过
	// classifyRetrieveErr——那个函数处理的是"检索过程中的数据库/上游故障"
	// 并带降级语义，把入参错误混进去会让一个明确的用户输入问题被当成可降级
	// 的基础设施抖动。
	//
	// ErrTooManyFilterDocuments 不截断而是报错：静默截断会悄悄改变调用方
	// 指定的范围，正是 FR-009 要防的事（见 model.go 的 maxFilterDocumentIDs）。
	ErrTooManyFilterDocuments = apperr.InvalidInput("knowledge.too_many_filter_documents", "指定的文档数量超出上限（最多 50 份），请缩小范围")
	ErrInvalidPageRange       = apperr.InvalidInput("knowledge.invalid_page_range", "页码范围不正确：页码必须为正整数，且起始页不得大于结束页")

	// ErrMetadataFilterDisabled：开关关闭时收到非空过滤器。这里明确报错而
	// 不是忽略过滤器照常检索——"我限定了范围，但系统用了范围外的资料回答"
	// 比"没找到"严重得多（spec Clarifications / research.md R4）。开关关闭
	// 时**空**过滤器不受影响，走的仍然是本功能上线前的那条路径。
	ErrMetadataFilterDisabled = apperr.InvalidInput("knowledge.metadata_filter_disabled", "检索元数据过滤未启用，无法按指定范围检索")

	ErrDocumentNotRetryable = apperr.Conflict("knowledge.document_not_retryable", "文档当前状态不支持重试，仅 pending/failed 状态可重试")
)
