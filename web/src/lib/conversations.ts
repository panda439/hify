import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@/lib/api";

export interface Conversation {
  id: string;
  agent_id: string;
  title: string;
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
