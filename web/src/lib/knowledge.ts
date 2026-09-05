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

export function useUploadDocument(kbId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (file: File) => {
      const form = new FormData();
      form.append("file", file);
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
