import { useMutation, useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@/lib/api";
import type { ChatModel } from "@/lib/agents";

export interface KnowledgeBase {
  id: string;
  name: string;
  description: string;
  embedding_model_id: string;
  chunk_size: number;
  chunk_overlap: number;
  is_active: boolean;
  total_chunks: number;
  max_chunks: number;
  created_at: string;
  updated_at: string;
}

interface KnowledgeBaseListResponse {
  items: KnowledgeBase[];
  total: number;
  page: number;
  page_size: number;
}

export interface CreateKnowledgeBaseInput {
  name: string;
  description?: string;
  embedding_model_id: string;
  chunk_size?: number;
  chunk_overlap?: number;
}

// name/description/is_active are the only editable fields — embedding
// model and chunking config are locked in at creation, see
// internal/knowledge/model.go's KnowledgeBase doc comment.
export interface UpdateKnowledgeBaseInput {
  name: string;
  description?: string;
  is_active: boolean;
}

const knowledgeBasesKey = ["knowledge-bases"] as const;

export function useKnowledgeBases() {
  return useQuery({
    queryKey: knowledgeBasesKey,
    queryFn: () => api.get<KnowledgeBaseListResponse>("/knowledge-bases?limit=100"),
  });
}

export function useCreateKnowledgeBase() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: CreateKnowledgeBaseInput) => api.post<KnowledgeBase>("/knowledge-bases", input),
    onSuccess: () => qc.invalidateQueries({ queryKey: knowledgeBasesKey }),
  });
}

export function useUpdateKnowledgeBase() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: UpdateKnowledgeBaseInput }) =>
      api.put<KnowledgeBase>(`/knowledge-bases/${id}`, input),
    onSuccess: () => qc.invalidateQueries({ queryKey: knowledgeBasesKey }),
  });
}

// Backs the knowledge base form's embedding-model picker — same /models
// endpoint the Agent form uses for chat models, just filtered differently.
export function useEmbeddingModels() {
  return useQuery({
    queryKey: ["models", "embedding"],
    queryFn: () => api.get<{ items: ChatModel[] }>("/models?capability=embedding"),
  });
}

export type DocumentStatus = "pending" | "processing" | "ready" | "failed";

export interface KnowledgeDocument {
  id: string;
  file_name: string;
  file_type: "txt" | "md" | "pdf";
  file_size: number;
  // 这份文档是否按场景切分（010）。上传时写定，之后不可改；存量文档恒为 false。
  is_narrative: boolean;
  is_relation_extraction_enabled: boolean;
  status: DocumentStatus;
  error_message: string;
  chunk_count: number;
  // 未能提取到文本的页码（1-indexed、升序）——典型来源是夹在电子文档中间的
  // 扫描页。null 或 [] 都表示「无提示」：后端只发 null 或非空数组，但两者都要
  // 当作无提示处理（契约 C2）。
  //
  // ⚠️ 它**不是错误**：携带它的文档 status 仍是 "ready"，可以正常检索，只是
  // 内容不完整。失败原因走 error_message，两条通道完全独立。
  //
  // ⚠️ 展示与否**只看 status**，不看这个字段有没有值：它可能是上一次成功处理
  // 留下的，而这一次处理失败了——那时显示它就是在描述一个文档已经不在的状态
  // （契约 C5）。
  unextracted_pages: number[] | null;
  // 根本没能被解析、被整页跳过的页码（008）。与 unextracted_pages **平行但不可
  // 互换**：两者分成两个字段的全部理由，就是用户的下一步不同——前者是「对这几页
  // 做 OCR」，后者是「**OCR 没用**，换个工具重新导出」。把两者并成一句
  // 「有 N 页没进去」，等于把唯一能告诉用户该干什么的那部分扔掉了。
  unparseable_pages: number[] | null;
  created_at: string;
  updated_at: string;
}

interface DocumentListResponse {
  items: KnowledgeDocument[];
  total: number;
  page: number;
  page_size: number;
}

function documentsQueryKey(kbId: string) {
  return ["knowledge-bases", kbId, "documents"];
}

// Polls while any document is still pending/processing — stops on its own
// once everything settles into ready/failed, so an idle knowledge base
// doesn't keep refetching forever.
export function useDocuments(kbId: string | null) {
  return useQuery({
    queryKey: kbId ? documentsQueryKey(kbId) : ["knowledge-bases", "documents", "disabled"],
    queryFn: () => api.get<DocumentListResponse>(`/knowledge-bases/${kbId}/documents?limit=100`),
    enabled: kbId !== null,
    refetchInterval: (query) => {
      const items = query.state.data?.items ?? [];
      const stillProcessing = items.some((d) => d.status === "pending" || d.status === "processing");
      return stillProcessing ? 1500 : false;
    },
  });
}

// 上传选项（010）。⭐ 省略等于全关，与这两个开关出现之前完全一致——
// 后端也是按「字段缺失 = 关闭」解析的，两边同一口径。
export interface UploadOptions {
  narrative?: boolean;
  // ⭐ 关系抽取是独立于 narrative 的第二个开关，且**必须同时给模型**
  // （010 T035）。留到"以后再配"等于让文档带着一个开着的开关停在那里
  // 什么都不做，而界面显示"已开启"——抽取必须调模型，没有模型就没有作业。
  extractRelations?: boolean;
  relationModelId?: string;
}

export function useUploadDocument(kbId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ file, options }: { file: File; options?: UploadOptions }) => {
      const form = new FormData();
      form.append("file", file);
      // ⚠️ 只在开启时才 append。后端只认精确的 "true"，其余一律当没勾；
      // 无脑 append String(false) 也能工作，但会让"没传"和"传了 false"
      // 在抓包和日志里长得不一样，排查时多一层噪音。
      if (options?.narrative) form.append("narrative_mode", "true");
      if (options?.extractRelations) {
        form.append("extract_relations", "true");
        // ⚠️ 模型 id 只在开关打开时才发。后端会在没有它时明确报错，
        // 前端也在表单上挡一道——两处都挡，是因为这里挡住的是
        // "用户看到一句中文提示"，后端挡住的是"数据进了库"。
        form.append("relation_model_id", options.relationModelId ?? "");
      }
      return api.postForm<KnowledgeDocument>(`/knowledge-bases/${kbId}/documents`, form);
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: documentsQueryKey(kbId) });
      qc.invalidateQueries({ queryKey: knowledgeBasesKey });
    },
  });
}

export function useDeleteDocument(kbId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (docId: string) => api.delete<void>(`/knowledge-bases/${kbId}/documents/${docId}`),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: documentsQueryKey(kbId) });
      qc.invalidateQueries({ queryKey: knowledgeBasesKey });
    },
  });
}

// --- 003-retrieval-playground: 试检索 ---

export interface RetrievalProbeInput {
  query: string;
  top_k?: number;
  document_ids?: string[];
  page_min?: number;
  page_max?: number;
}

export interface RetrievedChunkResult {
  id: string;
  document_id: string;
  document_name: string;
  // page_number / page_end 是这个片段覆盖的**页码闭区间**（006）：
  // page_number 是起始页、page_end 是结束页。null 表示这个片段没有页码——
  // txt/md 本来就没有，绝不是 0。
  //
  // ⚠️ 两者**要么同为 null、要么同有值**，后端由数据库约束
  // chunks_page_range_valid 强制（见 contracts/retrieval-page-range.md 的 R3）。
  // 因此前端**不得**写 `page_end ?? page_number` 之类的兜底：那只会把一个本该
  // 被发现的后端 bug 变成一个看起来正常的界面。
  page_number: number | null;
  page_end: number | null;
  content: string;
  score: number;
  // 邻接块：豁免页码过滤（见后端 002 的 FR-011），所以限定页码范围时
  // 仍然可能出现范围外的片段。界面必须把它和命中区分开，否则看起来像 bug。
  is_neighbor: boolean;
  neighbor_of: string;
}

export interface RetrievalProbeResult {
  chunks: RetrievedChunkResult[];
  hit_count: number;
  neighbor_count: number;
  filter_applied: boolean;
}

// 试检索是一次性查询，不产生任何持久化状态，因此不 invalidate 任何缓存。
export function useRetrievalProbe(kbId: string) {
  return useMutation({
    mutationFn: (input: RetrievalProbeInput) =>
      api.post<RetrievalProbeResult>(`/knowledge-bases/${kbId}/retrieve`, input),
  });
}

// --- 004-agent-document-scope ---

// useDocumentsByKnowledgeBase 并行拉取多个知识库各自的文档，供 Agent 表单
// 按知识库分组展示可勾选的文档。
//
// 返回**全部状态**的文档，不在这里按 status 过滤。调用方需要区分三种情况，
// 它们的处理方式完全不同：
//   - ready：可以勾选；
//   - pending/processing/failed：文档还在，只是当前没有已发布的分片。
//     勾了它检索不到东西，但它**不该**被当成"已删除"移除掉——重新处理完就好了；
//   - 完全查不到这个 id：文档已被删除。这才是需要提示用户清理的情况。
// 早期版本在这里就把非 ready 的滤掉了，导致调用方无法区分后两种，
// 会把一份正在处理的文档误判成已删除。
export function useDocumentsByKnowledgeBase(kbIds: string[]) {
  const results = useQueries({
    queries: kbIds.map((kbId) => ({
      queryKey: ["knowledge-bases", kbId, "documents"],
      queryFn: () => api.get<DocumentListResponse>(`/knowledge-bases/${kbId}/documents?limit=100`),
    })),
  });

  const byKnowledgeBase: Record<string, KnowledgeDocument[]> = {};
  const knownIds = new Set<string>();
  kbIds.forEach((kbId, i) => {
    const items = results[i]?.data?.items ?? [];
    byKnowledgeBase[kbId] = items;
    items.forEach((d) => knownIds.add(d.id));
  });

  const isLoading = results.some((r) => r.isLoading);

  return {
    byKnowledgeBase,
    // knownIds 是这些知识库下**当前存在**的全部文档 id（不分状态）。
    // 调用方用它判断某个已保存的范围 id 是不是已经失效。
    knownIds,
    isLoading,
    // 加载未完成时 knownIds 还不完整，此时任何"这个 id 不存在"的判断都不成立。
    // 单独暴露这个标志，避免调用方在加载过程中闪一下错误的"已删除"提示。
    canDetectMissing: !isLoading && kbIds.length > 0,
  };
}

// --- 010 T035：关系抽取的状态与操作 ---

// ExtractionStatus 是抽取作业的对外状态。
//
// ⚠️ 未知的数值一律是 null，**不是 0**。后端刻意这么发（见
// extraction_handler.go 的注释）：初始化还没完成时 total_items 填 0，
// 界面会显示"0/0 已完成"——一个看起来已经跑完的进度条，
// 而实际上一条都还没开始。前端必须跟着这个口径，不要 `?? 0`。
export interface ExtractionStatus {
  enabled: boolean;
  job_id: string | null;
  // state 是后端的作业状态原样。⚠️ **不要直接显示给用户**：
  // 它是 pending/initializing/running/... 这类内部词，
  // 展示文案走 extractionStateLabel。
  state: string | null;
  stop_reason: string | null;
  document_version: number | null;
  model_id: string | null;

  total_items: number | null;
  succeeded_items: number | null;
  failed_items: number | null;

  // ⭐ confirmed_calls 与 possible_calls 分开：前者"确定发生过"，
  // 后者含那些不知道有没有发出去的。合并会让一个不确定的数字
  // 看起来像确定的。
  confirmed_calls: number | null;
  possible_calls: number | null;
  unknown_usage_attempts: number | null;
  active_ms: number | null;

  remaining_calls: number | null;
  remaining_chunks: number | null;
  remaining_active_ms: number | null;

  // ⚠️ 本地模型没有金钱计费：cost_kind 恒为 not_applicable、
  // cost_amount 恒为 null。写 0 等于说"花了零元"，而真实情况是
  // 这个口径不适用。
  cost_kind: string;
  cost_amount: string | null;
}

// extractionStateLabel 把内部状态翻成用户看得懂的话。
//
// ⛔ 契约要求提示里**不出现 epoch / hash / lease / superseded** 这类内部
// 术语。用户不需要理解它们，出现在界面上只会让人以为自己需要理解。
// ⚠️ 每个状态给的下一步都不同，所以不能合并成"处理中"一个词：
// 「等待中」要等、「已暂停」要点续跑、「额度用尽」要加额度、「失败」要看原因。
export function extractionStateLabel(state: string | null): string {
  switch (state) {
    case "pending":
      return "等待文档处理完成";
    case "initializing":
      return "正在准备";
    case "running":
      return "正在抽取";
    case "paused":
      return "已暂停";
    case "succeeded":
      return "已完成";
    case "failed":
      return "已失败";
    case "budget_exhausted":
      return "额度已用尽";
    case "superseded":
      // 用户视角只有"这一轮不作数了"，不需要知道 superseded 这个词。
      return "已被新一轮取代";
    default:
      return "未开始";
  }
}

// extractionStopReasonLabel 把停止原因翻成一句人话。
// ⚠️ 认不出来的原因**原样不显示**，而不是显示英文码：一个用户看不懂的
// 标识符只会让他截图来问，而那句话本来就该由我们写。
export function extractionStopReasonLabel(reason: string | null): string {
  switch (reason) {
    case "budget_exhausted":
      return "已达到本次抽取的额度上限";
    case "paused_by_user":
      return "由你手动暂停";
    case "too_many_consecutive_failures":
      return "连续多个片段抽取失败，已停止";
    case "response_invalid":
      return "模型返回的结果不符合格式要求";
    case "input_too_large":
      return "有片段超出单次输入上限";
    case "source_changed":
      return "文档在抽取过程中被重新处理";
    case "chunk_metadata_missing":
      return "文档缺少场景坐标，请重新处理后再试";
    case "too_many_chunks":
      return "文档片段数超出单次抽取上限";
    case "empty_content":
      return "文档没有可抽取的正文";
    default:
      return "";
  }
}

// extractionProgress 返回 [已完成, 总数]，任一未知时返回 null。
//
// ⭐ 只要有一个是 null 就整体返回 null。⚠️ 把未知当 0 去算百分比，
// 会得到一个"0%"或"100%"的确定说法，而真相是我们还不知道。
export function extractionProgress(st: ExtractionStatus): [number, number] | null {
  if (st.total_items === null || st.succeeded_items === null || st.failed_items === null) {
    return null;
  }
  return [st.succeeded_items + st.failed_items, st.total_items];
}

function extractionKey(kbId: string, docId: string) {
  return ["knowledge-bases", kbId, "documents", docId, "extraction"];
}

// useExtractionStatus 轮询抽取状态。
//
// ⚠️ 只在**真的在跑**的时候轮询。pending 也要轮：它在等文档处理完成，
// 而那件事随时会完成。已完成/已暂停/已失败都不再轮——一个停着的作业
// 每 3 秒查一次，除了发热什么都不产生。
export function useExtractionStatus(kbId: string | null, docId: string | null, enabled: boolean) {
  return useQuery({
    queryKey: kbId && docId ? extractionKey(kbId, docId) : ["extraction", "disabled"],
    queryFn: () =>
      api.get<ExtractionStatus>(`/knowledge-bases/${kbId}/documents/${docId}/extraction`),
    enabled: enabled && kbId !== null && docId !== null,
    refetchInterval: (query) => {
      const st = query.state.data;
      if (!st) return false;
      const live = st.state === "pending" || st.state === "initializing" || st.state === "running";
      return live ? 3000 : false;
    },
  });
}

// ExtractionOperation 是所有写操作的请求体。
//
// ⚠️ 额度字段是**追加量**不是新上限。写成上限的话，一个填小了的值会把
// 已经用掉的额度算成超支，作业立刻停在"额度用尽"上。
export interface ExtractionOperation {
  idempotency_key: string;
  model_id?: string;
  additional_chunks?: number;
  additional_calls?: number;
  additional_active_seconds?: number;
  additional_retry_rounds?: number;
}

// newIdempotencyKey 给每次操作生成一个键。
//
// ⭐ 键在**用户点击的那一刻**生成并跟着这次操作走：重试同一次点击要用
// 同一个键（后端据此判定重放），而下一次点击必须是新键。
// ⚠️ 不能以 "upload:" 开头——那是后端给"上传时就开启"保留的前缀。
export function newIdempotencyKey(action: string): string {
  return `${action}-${crypto.randomUUID()}`;
}

type ExtractionAction = "enable" | "disable" | "pause" | "resume" | "restart";

export function useExtractionOperation(kbId: string, docId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ action, op }: { action: ExtractionAction; op: ExtractionOperation }) =>
      api.post<ExtractionStatus>(
        `/knowledge-bases/${kbId}/documents/${docId}/extraction/${action}`,
        op,
      ),
    onSuccess: (st) => {
      // ⭐ 直接把响应写进缓存：每个写操作都返回**操作之后**的状态，
      // 再查一次只会多一次往返，而且中间那一小段时间界面显示的是旧状态。
      qc.setQueryData(extractionKey(kbId, docId), st);
      qc.invalidateQueries({ queryKey: documentsQueryKey(kbId) });
    },
  });
}
