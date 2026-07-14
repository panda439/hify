import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@/lib/api";

export interface Agent {
  id: string;
  name: string;
  description: string;
  model_id: string;
  system_prompt: string;
  temperature: number;
  max_tokens: number | null;
  top_p: number | null;
  extra_params: Record<string, unknown> | null;
  knowledge_base_ids: string[] | null;
  is_active: boolean;
  created_by: string;
  created_at: string;
  updated_at: string;
}

interface AgentListResponse {
  items: Agent[];
  total: number;
  page: number;
  page_size: number;
}

// Temperature/max_tokens/top_p are optional here for the same reason the
// backend DTO uses pointers (see internal/agent/model.go): omitting a field
// must be distinguishable from explicitly sending 0.
export interface CreateAgentInput {
  name: string;
  description?: string;
  model_id: string;
  system_prompt?: string;
  temperature?: number;
  max_tokens?: number;
  top_p?: number;
  knowledge_base_ids?: string[];
}

// The backend replaces the whole row on update (no partial patch), so
// callers must pass every field.
export interface UpdateAgentInput {
  name: string;
  description?: string;
  model_id: string;
  system_prompt?: string;
  temperature?: number;
  max_tokens?: number;
  top_p?: number;
  knowledge_base_ids?: string[];
  is_active: boolean;
}

const agentsKey = ["agents"] as const;

export function useAgents() {
  return useQuery({
    queryKey: agentsKey,
    queryFn: () => api.get<AgentListResponse>("/agents?limit=100"),
  });
}

export function useCreateAgent() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: CreateAgentInput) => api.post<Agent>("/agents", input),
    onSuccess: () => qc.invalidateQueries({ queryKey: agentsKey }),
  });
}

export function useUpdateAgent() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: UpdateAgentInput }) =>
      api.put<Agent>(`/agents/${id}`, input),
    onSuccess: () => qc.invalidateQueries({ queryKey: agentsKey }),
  });
}

export interface ChatModel {
  id: string;
  provider_id: string;
  model_name: string;
  capability: "chat" | "embedding";
  context_window: number | null;
  max_output_tokens: number | null;
  is_default: boolean;
  is_active: boolean;
}

// Backs the model picker in the Agent form — GET /models is available to
// any authenticated user (unlike /providers, which is admin-only), since
// creating an Agent requires choosing a model but not managing providers.
export function useChatModels() {
  return useQuery({
    queryKey: ["models", "chat"],
    queryFn: () => api.get<{ items: ChatModel[] }>("/models?capability=chat"),
  });
}
