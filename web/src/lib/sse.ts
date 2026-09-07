// useChatStream hand-rolls SSE over fetch+ReadableStream instead of the
// browser's EventSource: sending the message body requires POST, which
// EventSource can't do. Errors that happen before the stream starts (e.g.
// the conversation doesn't exist) come back as a normal JSON error body;
// once the stream has started, errors arrive as an in-band "error" event
// from the backend — see internal/conversation/handler.go's SendMessage.
import { useCallback, useRef, useState } from "react";
import { createParser } from "eventsource-parser";
import { getAccessToken, refreshAccessToken } from "@/lib/api";

export interface RetrievedChunkInfo {
  knowledge_base_id: string;
  document_id: string;
  content: string;
  score: number;
}

// Fires twice per MCP tool invocation within a turn (running, then
// done|error) — see internal/conversation/model.go's ToolCallInfo.
export interface ToolCallInfo {
  name: string;
  status: "running" | "done" | "error";
  result?: string;
}

// RelationClarification 是关系查询命中多个同名人物时的候选集合（010）。
// ⚠️ 不用选的那一侧是空数组，前端据此只渲染一个选择器。
export interface RelationClarification {
  subject: string;
  object: string;
  document_id: string;
  subject_candidates: { character_id: string; display_name: string; context: string }[];
  object_candidates: { character_id: string; display_name: string; context: string }[];
}

export interface StreamEvent {
  type: "retrieval" | "tool_call" | "delta" | "final" | "relation_clarify" | "done" | "error";
  content?: string;
  error?: string;
  retrieved?: RetrievedChunkInfo[];
  tool_call?: ToolCallInfo;
  relation?: RelationClarification;
}

interface ErrorBody {
  error?: { message?: string };
}

// RelationQuery 是 010 的关系查询选项。
// ⚠️ 这里**没有**知识库/文档范围：范围由服务端从 Agent 配置里取。
// 前端能提交范围的话，"这个助手能查哪些书"就成了一个前端参数。
export interface RelationQuery {
  document_id: string;
  subject: string;
  object: string;
  // 上一轮返回歧义候选时，用户选定的人物。
  subject_character_id?: string;
  object_character_id?: string;
}

export interface SendOptions {
  relationQuery?: RelationQuery;
}

async function postStream(
  conversationId: string,
  content: string,
  token: string | null,
  signal: AbortSignal,
  options?: SendOptions,
) {
  return fetch(`/api/v1/conversations/${conversationId}/messages`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    credentials: "include",
    // ⚠️ 只在真的要查关系时才带这个字段：恒定发一个 null 会让"没查"和
    // "查了但没给参数"在抓包和后端日志里长得一样。
    body: JSON.stringify(
      options?.relationQuery ? { content, relation_query: options.relationQuery } : { content },
    ),
    signal,
  });
}

export function useChatStream() {
  const [streaming, setStreaming] = useState(false);
  const controllerRef = useRef<AbortController | null>(null);

  const send = useCallback(async (
    conversationId: string,
    content: string,
    onEvent: (event: StreamEvent) => void,
    options?: SendOptions,
  ) => {
    const controller = new AbortController();
    controllerRef.current = controller;
    setStreaming(true);

    try {
      let res = await postStream(conversationId, content, getAccessToken(), controller.signal, options);
      if (res.status === 401) {
        const user = await refreshAccessToken();
        if (user) {
          res = await postStream(conversationId, content, getAccessToken(), controller.signal, options);
        }
      }

      if (!res.ok || !res.body) {
        const body: ErrorBody | null = await res.json().catch(() => null);
        onEvent({ type: "error", error: body?.error?.message ?? "请求失败，请重试" });
        return;
      }

      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      const parser = createParser({
        onEvent(event) {
          if (!event.data) return;
          try {
            onEvent(JSON.parse(event.data) as StreamEvent);
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
      setStreaming(false);
      controllerRef.current = null;
    }
  }, []);

  const stop = useCallback(() => {
    controllerRef.current?.abort();
  }, []);

  return { send, stop, streaming };
}
