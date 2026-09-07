package knowledge

import (
	"errors"

	"hify/internal/platform/apperr"
)

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
	//
	// 006-pdf-layout-chunking：ErrEmptyContent 的**文案一个字不改**，但适用
	// 范围收缩为"真正的空文件"——零字节、只有空白、解析后确实一个字都没有。
	// 在此之前它同时兜住了扫描件，那让用户无法区分"我传了个空文件"和"我传了
	// 份需要 OCR 的扫描件"，两者的下一步动作完全不同（SC-007）。
	ErrEmptyContent           = apperr.InvalidInput("knowledge.empty_content", "文档内容为空或无法提取到文本")
	ErrEmbeddingCountMismatch = apperr.InvalidInput("knowledge.embedding_count_mismatch", "向量生成数量与分块数量不一致，请重试")
	ErrTooManyChunks          = apperr.InvalidInput("knowledge.too_many_chunks", "文档分块数量超出单文档上限，请拆分文件后重新上传")

	// ErrPDFNoTextLayer fires when a PDF has no extractable text on ANY
	// page — the scanned/image-only case. Split out of ErrEmptyContent by
	// 006-pdf-layout-chunking for one reason: turning a confusing failure
	// into an actionable one. "文档内容为空或无法提取到文本" is accurate but
	// leaves the user with nothing to do; naming OCR tells them exactly
	// what the next step is, without reading any log (SC-007).
	//
	// Classified InvalidInput, same as ErrEmptyContent: this is "the file
	// itself is unsuitable", not an infrastructure fault — retrying it
	// unchanged will produce the same result every time.
	//
	// ⚠️ No OCR and no visual retrieval in this phase (FR-019). This error
	// is the whole of the scanned-PDF work: recognising the boundary of
	// what the system can do and degrading explicitly, rather than pretending
	// the file was empty.
	//
	// Message carries no dynamic content (how many pages, which pages) —
	// those go to the structured log, per the rule above.
	ErrPDFNoTextLayer = apperr.InvalidInput("knowledge.pdf_no_text_layer",
		"该 PDF 没有文本层（疑似扫描件或图片型 PDF），暂不支持自动识别，请先用 OCR 工具转换为可选中文字的 PDF 后重新上传")

	// ErrPDFUnreadable is what a PDF the parser cannot read at all becomes.
	//
	// ⚠️ Found by 006's own acceptance step (quickstart §5: verify against a
	// REAL multi-page PDF, not one this repo's test helper built), and it is
	// a PRE-EXISTING defect, not one 006 introduced: rsc.io/pdf PANICS on
	// malformed or unsupported constructs — "malformed PDF: reading at
	// offset 0: stream not present" is what two ordinary arXiv papers
	// produced — and nothing in this package recovered. The failure was
	// therefore not "this file could not be processed" but a panic thrown
	// out of document processing.
	//
	// Turning that into an error is the same move US5 makes for scanned
	// PDFs, for the same reason: the system genuinely cannot handle this
	// file, and the honest response is to say so in a sentence the user can
	// act on — not to crash, and not to leave the document stuck.
	//
	// ⚠️ This does NOT make those PDFs work. rsc.io/pdf's coverage of
	// real-world PDFs is the underlying problem and it is untouched here;
	// replacing the parser is exactly the "swap the parsing layer" route
	// research.md R1 weighed and rejected. This error is a containment, and
	// the acceptance report must describe it as one.
	ErrPDFUnreadable = apperr.InvalidInput("knowledge.pdf_unreadable",
		"无法解析该 PDF 文件（文件可能已损坏，或使用了当前解析器不支持的格式），请尝试用其他工具重新导出后上传")

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

// --- 010-narrative-scene-chunking-and-relation-extraction ---

// ErrNarrativeUnsupportedFileType rejects formats without a narrative parser.
var ErrNarrativeUnsupportedFileType = apperr.InvalidInput(
	"knowledge.narrative_unsupported_file_type",
	"按场景分块只支持 txt、md 和可解析的 pdf 文件")

// ErrRelationExtractionModelNotConfigured：没有配置抽取用的模型。
//
// ⚠️ 宁可报错也不静默接受。接受并存下这个开关、却什么都不发生，
// 用户会一直等一个永远不会开始的抽取，而系统不会说任何话。
var ErrRelationExtractionModelNotConfigured = apperr.InvalidInput(
	"knowledge.relation_extraction_model_not_configured",
	"服务端没有配置关系抽取使用的模型，请联系管理员")

// ErrNarrativeMetadataInvalid：来源映射自检没过，文档直接判失败。
//
// ⚠️ 这条是给**我们自己**的守卫，不是给用户的输入校验，所以它是唯一一个
// "正常情况下永远不该出现"的 010 错误。validateNarrativeMetadata 检查的是
// 区间覆盖、长度一致、不可引用段不带坐标这类不变量——全都没有运行时症状：
// 映射错了的块照样能嵌入、照样能被检索到，只是它给出的引用指向原文的错误
// 位置，而且看上去完全合理。宁可让这份文档 failed（用户能看见、能重试），
// 也不要把一份坐标错位的证据悄悄发布出去。
// 刻意**不是** apperr：这不是用户能修的输入问题，是我们自己的不变量破了。
// 走 userFacingFailureMessage 的兜底分支，用户看到通用的"处理失败"提示，
// 而真正的细节（哪一块、哪一段、坐标是什么）进 slog.Error 给我们看。
var errNarrativeMetadataInvalid = errors.New("knowledge: narrative source mapping failed self-check")

// ErrRelationExtractionRequiresNarrative 与 000017 的 CHECK 约束同义，
// 在 Service 层先挡一道，让用户拿到中文提示而不是数据库错误。
var ErrRelationExtractionRequiresNarrative = apperr.InvalidInput(
	"knowledge.relation_extraction_requires_narrative",
	"关系抽取只能在开启按场景分块的文档上使用")
