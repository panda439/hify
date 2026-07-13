import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@/lib/api";

export interface ExtraConfig {
  idle_timeout_seconds?: number;
  max_concurrent?: number;
  rate_limit_per_minute?: number;
}

export type AuthType = "api_key" | "none";
export type TestStatus = "unknown" | "success" | "failed";

export interface Provider {
  id: string;
  name: string;
  adapter_type: string;
  base_url: string;
  auth_type: AuthType;
  has_api_key: boolean;
  extra_headers: Record<string, string> | null;
  extra_config: ExtraConfig;
  last_tested_at: string | null;
  last_test_status: TestStatus;
  last_test_error: string;
  is_active: boolean;
  created_at: string;
  updated_at: string;
}

export interface ProviderModel {
  id: string;
  provider_id: string;
  model_name: string;
  capability: "chat" | "embedding";
  context_window: number | null;
  max_output_tokens: number | null;
  embedding_dimension: number | null;
  is_default: boolean;
  is_active: boolean;
}

interface ProviderListResponse {
  items: Provider[];
  total: number;
  page: number;
  page_size: number;
}

export interface CreateProviderInput {
  name: string;
  base_url: string;
  auth_type: AuthType;
  api_key?: string;
}

export interface UpdateProviderInput {
  name: string;
  base_url: string;
  auth_type: AuthType;
  api_key?: string | null; // undefined/omitted = unchanged, "" = clear (only valid for auth_type=none)
  is_active: boolean;
}

const providersKey = ["providers"] as const;

export function useProviders() {
  return useQuery({
    queryKey: providersKey,
    queryFn: () => api.get<ProviderListResponse>("/providers?limit=100"),
  });
}

export function useCreateProvider() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: CreateProviderInput) => api.post<Provider>("/providers", input),
    onSuccess: () => qc.invalidateQueries({ queryKey: providersKey }),
  });
}

export function useUpdateProvider() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: UpdateProviderInput }) =>
      api.put<Provider>(`/providers/${id}`, input),
    onSuccess: () => qc.invalidateQueries({ queryKey: providersKey }),
  });
}

export function useTestConnection() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.post<void>(`/providers/${id}/test`),
    onSettled: () => qc.invalidateQueries({ queryKey: providersKey }),
  });
}

export function useProviderModels(providerId: string | null) {
  return useQuery({
    queryKey: ["providers", providerId, "models"],
    queryFn: () => api.get<{ items: ProviderModel[] }>(`/providers/${providerId}/models`),
    enabled: providerId !== null,
  });
}

export interface AddModelInput {
  model_name: string;
  capability: "chat" | "embedding";
  context_window?: number;
  embedding_dimension?: number;
  is_default?: boolean;
}

export function useAddModel(providerId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: AddModelInput) => api.post<ProviderModel>(`/providers/${providerId}/models`, input),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["providers", providerId, "models"] }),
  });
}

// The backend replaces the whole row on update (no partial patch), so
// callers must pass every field — see toggleModelField in
// provider-models-dialog.tsx, which fills these in from the current row.
export interface UpdateModelInput {
  context_window?: number;
  max_output_tokens?: number;
  embedding_dimension?: number;
  is_default: boolean;
  is_active: boolean;
}

export function useUpdateModel(providerId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: UpdateModelInput }) =>
      api.put<ProviderModel>(`/providers/${providerId}/models/${id}`, input),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["providers", providerId, "models"] }),
  });
}
