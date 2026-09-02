import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
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
  // null 表示这个片段没有页码——txt/md 本来就没有，绝不是 0。界面显示为「—」。
  page_number: number | null;
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
