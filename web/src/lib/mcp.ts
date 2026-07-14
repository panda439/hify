import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@/lib/api";

export type Transport = "stdio" | "sse";
export type ServerStatus = "unknown" | "connected" | "failed";

export interface McpServer {
  id: string;
  name: string;
  transport: Transport;
  command: string;
  args: string[] | null;
  env_keys: string[] | null;
  url: string;
  header_keys: string[] | null;
  status: ServerStatus;
  last_synced_at: string | null;
  last_error: string;
  is_active: boolean;
  created_at: string;
  updated_at: string;
}

export interface McpTool {
  id: string;
  mcp_server_id: string;
  tool_name: string;
  description: string;
  input_schema: Record<string, unknown>;
  is_active: boolean;
}

interface McpServerListResponse {
  items: McpServer[];
  total: number;
  page: number;
  page_size: number;
}

export interface CreateMcpServerInput {
  name: string;
  transport: Transport;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  url?: string;
  headers?: Record<string, string>;
}

// env/headers omitted (not just an empty object) means "leave unchanged" —
// the API only ever returns key names back (see McpServer.env_keys), never
// values, so there's no way to prefill and resend them; see the doc
// comment on internal/mcp/service.go's UpdateServer.
export interface UpdateMcpServerInput {
  name: string;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  url?: string;
  headers?: Record<string, string>;
  is_active: boolean;
}

const mcpServersKey = ["mcp-servers"] as const;

export function useMcpServers() {
  return useQuery({
    queryKey: mcpServersKey,
    queryFn: () => api.get<McpServerListResponse>("/mcp-servers?limit=100"),
  });
}

export function useCreateMcpServer() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: CreateMcpServerInput) => api.post<McpServer>("/mcp-servers", input),
    onSuccess: () => qc.invalidateQueries({ queryKey: mcpServersKey }),
  });
}

export function useUpdateMcpServer() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: UpdateMcpServerInput }) =>
      api.put<McpServer>(`/mcp-servers/${id}`, input),
    onSuccess: () => qc.invalidateQueries({ queryKey: mcpServersKey }),
  });
}

export function useSyncMcpTools(serverId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.post<{ items: McpTool[] }>(`/mcp-servers/${serverId}/sync`),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: [...mcpServersKey, serverId, "tools"] });
      qc.invalidateQueries({ queryKey: mcpServersKey });
    },
  });
}

export function useMcpServerTools(serverId: string | null) {
  return useQuery({
    queryKey: [...mcpServersKey, serverId, "tools"],
    queryFn: () => api.get<{ items: McpTool[] }>(`/mcp-servers/${serverId}/tools`),
    enabled: serverId !== null,
  });
}

// Backs the Agent form's tool picker — any authenticated user, not just
// admins, mirroring provider's "/models" carve-out.
export function useActiveMcpTools() {
  return useQuery({
    queryKey: ["mcp-tools"],
    queryFn: () => api.get<{ items: McpTool[] }>("/mcp-tools"),
  });
}
