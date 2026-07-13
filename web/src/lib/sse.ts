// useChatStream hand-rolls SSE over fetch+ReadableStream instead of the
// browser's EventSource: sending the message body requires POST, which
// EventSource can't do. Errors that happen before the stream starts (e.g.
// the conversation doesn't exist) come back as a normal JSON error body;
// once the stream has started, errors arrive as an in-band "error" event
// from the backend — see internal/conversation/handler.go's SendMessage.
import { useCallback, useRef, useState } from "react";
import { createParser } from "eventsource-parser";
import { getAccessToken, refreshAccessToken } from "@/lib/api";

export interface StreamEvent {
  type: "delta" | "done" | "error";
  content?: string;
  error?: string;
}

interface ErrorBody {
  error?: { message?: string };
}

async function postStream(conversationId: string, content: string, token: string | null, signal: AbortSignal) {
  return fetch(`/api/v1/conversations/${conversationId}/messages`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    credentials: "include",
    body: JSON.stringify({ content }),
    signal,
  });
}

export function useChatStream() {
  const [streaming, setStreaming] = useState(false);
  const controllerRef = useRef<AbortController | null>(null);

  const send = useCallback(async (conversationId: string, content: string, onEvent: (event: StreamEvent) => void) => {
    const controller = new AbortController();
    controllerRef.current = controller;
    setStreaming(true);

    try {
      let res = await postStream(conversationId, content, getAccessToken(), controller.signal);
      if (res.status === 401) {
        const user = await refreshAccessToken();
        if (user) {
          res = await postStream(conversationId, content, getAccessToken(), controller.signal);
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
