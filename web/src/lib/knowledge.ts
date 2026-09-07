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
  is_narrative?: boolean;
  is_relation_extraction_enabled?: boolean;
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
  // ⚠️ 抽取必须先开叙事：后端会拒绝 extract_relations=true 而 narrative=false，
  // 界面上也要把这个依赖表现出来，不能让用户勾了之后才被后端拒绝。
  extractRelations?: boolean;
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
      if (options?.extractRelations) form.append("extract_relations", "true");
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

// --- 010：关系抽取的状态与控制 ---

// ⚠️ 三个可空字段是"未知"，**不是 0**。后端明确发 null：一个显示 0/0 的
// 进度条看起来像"跑完了，什么都没有"，而事实是"还不知道有多少"。
// cost_amount 同理——本地模型没有金钱计费，显示 ¥0 会被读成免费。
export interface ExtractionStatus {
  enabled: boolean;
  job_id?: string;
  document_version?: number;
  state?: "initializing" | "running" | "paused" | "succeeded" | "failed" | "superseded";
  stop_reason?: string;
  run_number?: number;
  model_id?: string;
  total_items: number | null;
  succeeded_items: number;
  failed_items: number;
  has_partial_evidence: boolean;
  confirmed_calls: number;
  possible_calls: number;
  unknown_usage_attempts: number;
  active_ms: number;
  wall_ms: number | null;
  cost_kind: string;
  cost_amount: number | null;
  remaining_calls: number;
  remaining_chunks: number | null;
  remaining_active_ms: number;
  retry_rounds: number;
}

function extractionKey(kbId: string, docId: string) {
  return ["knowledge-bases", kbId, "documents", docId, "extraction"];
}

// 只在面板展开时才查（enabled 控制），并且**按文档单查**：
// 列表接口不带抽取状态是有意的——每份文档一次查询会把文档列表变成 N+1。
export function useExtractionStatus(kbId: string, docId: string, enabled: boolean) {
  return useQuery({
    queryKey: extractionKey(kbId, docId),
    queryFn: () =>
      api.get<ExtractionStatus>(`/knowledge-bases/${kbId}/documents/${docId}/extraction`),
    enabled,
    // 跑着的时候盯紧一点，停下来就不再轮询。
    refetchInterval: (query) => {
      const state = query.state.data?.state;
      return state === "running" || state === "initializing" ? 2000 : false;
    },
  });
}

export type ExtractionAction = "enable" | "disable" | "pause" | "resume" | "restart";

export interface ExtractionActionBody {
  additional_calls?: number;
  additional_chunks?: number;
  additional_active_seconds?: number;
  additional_retry_rounds?: number;
  model_id?: string;
}

// ⭐ 幂等键在**发起这次动作时**生成一次，重试用同一个键。
// 追加额度是累加的：没有键的话，一次网络重试就多加一份额度，
// 而两次响应都显示成功。
export function useExtractionAction(kbId: string, docId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ action, body }: { action: ExtractionAction; body?: ExtractionActionBody }) =>
      api.post<ExtractionStatus>(
        `/knowledge-bases/${kbId}/documents/${docId}/extraction/${action}`,
        { idempotency_key: crypto.randomUUID(), ...body },
      ),
    onSuccess: (status) => {
      qc.setQueryData(extractionKey(kbId, docId), status);
      qc.invalidateQueries({ queryKey: documentsQueryKey(kbId) });
    },
  });
}
