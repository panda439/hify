import { useCallback, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createParser } from "eventsource-parser";
import { api, getAccessToken, refreshAccessToken } from "@/lib/api";

export type StepType = "start" | "end" | "llm_call" | "knowledge_retrieval" | "conditional" | "tool_call";

// Config shapes match internal/workflow/executor.go's *Config structs
// verbatim (json tags) — kept loose (Record) rather than a discriminated
// union since the canvas editor reads/writes one type's fields at a time
// based on the node's own `type`.
export interface Step {
  id: string;
  type: StepType;
  config?: Record<string, unknown>;
  next?: string;
  next_if_true?: string;
  next_if_false?: string;
}

export interface Definition {
  steps: Step[];
}

export interface Workflow {
  id: string;
  name: string;
  description: string;
  definition: Definition;
  is_active: boolean;
  created_by: string;
  created_at: string;
  updated_at: string;
}

interface WorkflowListResponse {
  items: Workflow[];
  total: number;
  page: number;
  page_size: number;
}

export interface CreateWorkflowInput {
  name: string;
  description?: string;
  definition: Definition;
}

export interface UpdateWorkflowInput {
  name: string;
  description?: string;
  definition: Definition;
  is_active: boolean;
}

export type RunStatus = "running" | "succeeded" | "failed";

export interface WorkflowRun {
  id: string;
  workflow_id: string;
  status: RunStatus;
  input: string;
  output: string;
  error_message: string;
  started_at: string;
  finished_at: string | null;
  created_by: string;
}

export type StepStatus = "succeeded" | "failed";

export interface WorkflowRunStep {
  id: string;
  workflow_run_id: string;
  step_id: string;
  step_type: StepType;
  status: StepStatus;
  input: string;
  output: string;
  error_message: string;
  started_at: string;
  finished_at: string | null;
}

const workflowsKey = ["workflows"] as const;

export function useWorkflows() {
  return useQuery({
    queryKey: workflowsKey,
    queryFn: () => api.get<WorkflowListResponse>("/workflows?limit=100"),
  });
}

export function useWorkflow(id: string | null) {
  return useQuery({
    queryKey: [...workflowsKey, id],
    queryFn: () => api.get<Workflow>(`/workflows/${id}`),
    enabled: id !== null,
  });
}

export function useCreateWorkflow() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: CreateWorkflowInput) => api.post<Workflow>("/workflows", input),
    onSuccess: () => qc.invalidateQueries({ queryKey: workflowsKey }),
  });
}

export function useUpdateWorkflow() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: UpdateWorkflowInput }) =>
      api.put<Workflow>(`/workflows/${id}`, input),
    onSuccess: (_data, vars) => {
      qc.invalidateQueries({ queryKey: workflowsKey });
      qc.invalidateQueries({ queryKey: [...workflowsKey, vars.id] });
    },
  });
}

export function useWorkflowRuns(workflowId: string | null) {
  return useQuery({
    queryKey: [...workflowsKey, workflowId, "runs"],
    queryFn: () => api.get<{ items: WorkflowRun[]; total: number }>(`/workflows/${workflowId}/runs?limit=20`),
    enabled: workflowId !== null,
  });
}

export function useWorkflowRunSteps(workflowId: string | null, runId: string | null) {
  return useQuery({
    queryKey: [...workflowsKey, workflowId, "runs", runId, "steps"],
    queryFn: () => api.get<{ items: WorkflowRunStep[] }>(`/workflows/${workflowId}/runs/${runId}/steps`),
    enabled: workflowId !== null && runId !== null,
  });
}

// Mirrors lib/sse.ts's useChatStream: fetch+ReadableStream instead of
// EventSource because triggering a run is a POST with a body. Terminal
// events are "done"/"error" — see internal/workflow/model.go's
// EventDone/EventError.
export interface WorkflowStepEvent {
  type: "step" | "done" | "error";
  run_id?: string;
  step_id?: string;
  step_type?: StepType;
  status?: "running" | "succeeded" | "failed";
  output?: string;
  error?: string;
}

async function postExecute(workflowId: string, input: string, token: string | null, signal: AbortSignal) {
  return fetch(`/api/v1/workflows/${workflowId}/executions`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    credentials: "include",
    body: JSON.stringify({ input }),
    signal,
  });
}

export function useWorkflowExecution() {
  const [running, setRunning] = useState(false);
  const controllerRef = useRef<AbortController | null>(null);

  const execute = useCallback(
    async (workflowId: string, input: string, onEvent: (event: WorkflowStepEvent) => void) => {
      const controller = new AbortController();
      controllerRef.current = controller;
      setRunning(true);

      try {
        let res = await postExecute(workflowId, input, getAccessToken(), controller.signal);
        if (res.status === 401) {
          const user = await refreshAccessToken();
          if (user) {
            res = await postExecute(workflowId, input, getAccessToken(), controller.signal);
          }
        }

        if (!res.ok || !res.body) {
          const body: { error?: { message?: string } } | null = await res.json().catch(() => null);
          onEvent({ type: "error", error: body?.error?.message ?? "请求失败，请重试" });
          return;
        }

        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        const parser = createParser({
          onEvent(event) {
            if (!event.data) return;
            try {
              onEvent(JSON.parse(event.data) as WorkflowStepEvent);
            } catch {
              // malformed frame — ignore rather than crash the stream
            }
          },
        });

        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          parser.feed(decoder.decode(value, { stream: true }));
        }
      } catch (err) {
        if ((err as Error).name !== "AbortError") {
          onEvent({ type: "error", error: "连接中断，请重试" });
        }
      } finally {
        setRunning(false);
        controllerRef.current = null;
      }
    },
    [],
  );

  const stop = useCallback(() => {
    controllerRef.current?.abort();
  }, []);

  return { execute, stop, running };
}
