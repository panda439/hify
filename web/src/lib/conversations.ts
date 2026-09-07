import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@/lib/api";

export interface Conversation {
  id: string;
  agent_id: string;
  title: string;
  last_message: string;
  created_at: string;
  updated_at: string;
}

export interface Message {
  id: string;
  role: "system" | "user" | "assistant" | "tool";
  content: string;
  token_count: number;
  created_at: string;
}

interface ConversationListResponse {
  items: Conversation[];
  total: number;
  page: number;
  page_size: number;
}

interface MessagePage {
  items: Message[];
  next_cursor?: string;
}

const conversationsKey = ["conversations"] as const;

export function useConversations() {
  return useQuery({
    queryKey: conversationsKey,
    queryFn: () => api.get<ConversationListResponse>("/conversations?limit=100"),
  });
}

export function useCreateConversation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (agentId: string) => api.post<Conversation>("/conversations", { agent_id: agentId }),
    onSuccess: () => qc.invalidateQueries({ queryKey: conversationsKey }),
  });
}

// A single bounded fetch of the most recent messages — matches the
// backend's two-layer truncation bound. "Load more" older history isn't
// wired up yet; the cursor endpoint is there for when that's needed.
export function useMessages(conversationId: string | null) {
  return useQuery({
    queryKey: ["conversations", conversationId, "messages"],
    queryFn: () => api.get<MessagePage>(`/conversations/${conversationId}/messages?limit=200`),
    enabled: conversationId !== null,
  });
}

export function messagesQueryKey(conversationId: string) {
  return ["conversations", conversationId, "messages"];
}

export function conversationsQueryKey() {
  return conversationsKey;
}

// --- 010 T035：关系提问 ---

// RelationDocument 是一本"可以问人物关系的书"。
//
// ⛔ 后端只发用户看得懂的字段：没有 job_id、没有 epoch、没有 hash。
// 前端也不要凭空造出这类术语。
export interface RelationDocument {
  document_id: string;
  file_name: string;
  // ready 表示"现在问就能得到基于全书的答案"。
  // ⚠️ 它**不是**文档的处理状态：抽取跑完才算，而抽取比解析晚得多。
  ready: boolean;
  remaining_chunks: number;
  // stopped 表示这一轮停了（暂停 / 额度用尽 / 失败）。
  // ⚠️ 与"还没跑完"分开显示：前者要去知识库页点续跑，后者只要等。
  stopped: boolean;
}

// relationDocumentHint 是书目旁边那句话。
//
// ⚠️ 三种情况必须给不同的话：没跑完要等、停了要去续跑、跑完了才可以放心问。
// 合并成一句"抽取中"会让用户在一个已经停住的作业上一直等下去。
export function relationDocumentHint(d: RelationDocument): string {
  if (d.stopped) return "这本书的抽取已停止，去知识库页可以继续";
  if (!d.ready) {
    return d.remaining_chunks > 0
      ? `还有 ${d.remaining_chunks} 个片段没处理，现在提问只覆盖已处理的部分`
      : "抽取还没完成，现在提问只覆盖已处理的部分";
  }
  return "";
}

export function useRelationDocuments(conversationId: string | null, enabled: boolean) {
  return useQuery({
    queryKey: conversationId
      ? ["conversations", conversationId, "relation-documents"]
      : ["relation-documents", "disabled"],
    queryFn: () =>
      api.get<{ items: RelationDocument[] }>(`/conversations/${conversationId}/relation-documents`),
    enabled: enabled && conversationId !== null,
  });
}

// RelationQueryInput 是一次关系提问的三个要素。
//
// ⭐ 由用户**显式**选书、填两个名字，不从提问里猜意图。⚠️ 猜错的两个方向
// 后果都不小：该走关系分支却走了普通检索，用户得到一个基于零散片段的含糊
// 回答；不该走却走了，一个普通问题被答成"没有找到这两个人的关系记录"。
// 而无论哪一种，用户都看不出系统做过一次判断。
export interface RelationQueryInput {
  document_id: string;
  subject: string;
  object: string;
}
