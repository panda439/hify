import { useEffect, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { MessageSquare, Plus, Send, Square } from "lucide-react";
import { marked } from "marked";
import DOMPurify from "dompurify";

import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { EmptyState } from "@/components/empty-state";
import { cn } from "@/lib/utils";
import { useAgents } from "@/lib/agents";
import {
  conversationsQueryKey,
  messagesQueryKey,
  useConversations,
  useMessages,
  type Message,
} from "@/lib/conversations";
import { useChatStream } from "@/lib/sse";
import { NewConversationDialog } from "@/routes/new-conversation-dialog";

// A message shown in the transcript while it's still in flight — not yet
// the persisted row the backend returns, so it only needs what the bubble
// renders (see Message in lib/conversations.ts for the persisted shape).
// error is set when the stream fails (request-level failure or an in-band
// "error" event) — the bubble stays on screen showing whatever content did
// stream in, plus the error, rather than disappearing.
interface PendingMessage {
  id: string;
  role: "user" | "assistant";
  content: string;
  error?: string;
}

function formatRelativeTime(iso: string): string {
  const date = new Date(iso);
  const now = new Date();
  const diffMin = Math.floor((now.getTime() - date.getTime()) / 60000);

  if (diffMin < 1) return "刚刚";
  if (diffMin < 60) return `${diffMin} 分钟前`;
  if (date.toDateString() === now.toDateString()) return `${Math.floor(diffMin / 60)} 小时前`;

  const yesterday = new Date(now);
  yesterday.setDate(now.getDate() - 1);
  if (date.toDateString() === yesterday.toDateString()) return "昨天";

  const diffDay = Math.floor(diffMin / 1440);
  if (diffDay < 7) return `${diffDay} 天前`;
  return date.toLocaleDateString("zh-CN", { month: "numeric", day: "numeric" });
}

const MESSAGE_PREVIEW_MAX_CHARS = 30;

function truncatePreview(text: string): string {
  return text.length > MESSAGE_PREVIEW_MAX_CHARS
    ? text.slice(0, MESSAGE_PREVIEW_MAX_CHARS) + "…"
    : text;
}

export function ChatPage() {
  const { data: conversationsData } = useConversations();
  const { data: agentsData } = useAgents();
  const conversations = conversationsData?.items ?? [];
  const agentNameById = new Map((agentsData?.items ?? []).map((a) => [a.id, a.name]));

  const [conversationId, setConversationId] = useState<string | null>(null);
  const [newDialogOpen, setNewDialogOpen] = useState(false);
  const [draft, setDraft] = useState("");
  const [pending, setPending] = useState<PendingMessage[]>([]);
  const { send, stop, streaming } = useChatStream();
  const qc = useQueryClient();

  const { data: messagesData } = useMessages(conversationId);
  const persisted = messagesData?.items ?? [];

  const scrollRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    scrollRef.current?.scrollIntoView({ block: "end" });
  }, [persisted, pending]);

  const handleSelectConversation = (id: string) => {
    if (streaming) return;
    setConversationId(id);
    setPending([]);
  };

  const handleSend = async () => {
    const content = draft.trim();
    if (!content || !conversationId || streaming) return;
    setDraft("");
    setPending([
      { id: "pending-user", role: "user", content },
      { id: "pending-assistant", role: "assistant", content: "" },
    ]);

    await send(conversationId, content, async (event) => {
      if (event.type === "delta") {
        setPending((prev) =>
          prev.map((m) => (m.id === "pending-assistant" ? { ...m, content: m.content + (event.content ?? "") } : m)),
        );
      } else if (event.type === "done") {
        // Wait for the refetch before dropping the pending overlay, so the
        // just-finished reply doesn't flash empty for a frame while the
        // persisted list catches up.
        await qc.invalidateQueries({ queryKey: messagesQueryKey(conversationId) });
        qc.invalidateQueries({ queryKey: conversationsQueryKey() });
        setPending([]);
      } else if (event.type === "error") {
        setPending((prev) =>
          prev.map((m) =>
            m.id === "pending-assistant" ? { ...m, error: event.error ?? "对话出错，请重试" } : m,
          ),
        );
        // Whatever partial content the backend already persisted (see
        // conversation/service.go's runStream) will show up correctly next
        // time this conversation loads — refetch in the background, but
        // don't race it against the inline error bubble staying visible.
        qc.invalidateQueries({ queryKey: messagesQueryKey(conversationId) });
        qc.invalidateQueries({ queryKey: conversationsQueryKey() });
      }
    });
  };

  const handleKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      void handleSend();
    }
  };

  return (
    <div className="-m-6 flex h-[calc(100svh-0px)]">
      <div className="flex w-[260px] shrink-0 flex-col border-r bg-muted/30 p-3">
        {conversations.length === 0 ? (
          <div className="flex flex-1 items-center justify-center text-center text-sm text-muted-foreground">
            还没有对话记录
          </div>
        ) : (
          <div className="flex flex-1 flex-col gap-1 overflow-y-auto">
            {conversations.map((c) => (
              <button
                key={c.id}
                onClick={() => handleSelectConversation(c.id)}
                className={cn(
                  "rounded-md px-3 py-2 text-left text-sm hover:bg-muted",
                  c.id === conversationId && "bg-muted font-medium",
                )}
              >
                <div className="truncate">
                  {c.title || agentNameById.get(c.agent_id) || "对话"}
                  <span className="text-muted-foreground"> · {formatRelativeTime(c.updated_at)}</span>
                </div>
                {c.last_message && (
                  <div className="truncate text-xs text-muted-foreground">{truncatePreview(c.last_message)}</div>
                )}
              </button>
            ))}
          </div>
        )}
        <Button className="mt-3 justify-start" variant="outline" onClick={() => setNewDialogOpen(true)}>
          <Plus />
          新建对话
        </Button>
      </div>

      <div className="flex flex-1 flex-col">
        {conversationId === null ? (
          <div className="flex flex-1 items-center justify-center p-6">
            <EmptyState
              icon={<MessageSquare className="size-5" />}
              title="选择一个 Agent 开始对话"
              description="从左侧新建对话，或者先到「Agent 管理」创建一个 Agent"
            />
          </div>
        ) : (
          <>
            <div className="flex-1 overflow-y-auto p-6">
              <div className="mx-auto flex max-w-3xl flex-col gap-4">
                {[...persisted, ...pending].map((m) => (
                  <MessageBubble key={m.id} message={m} />
                ))}
                <div ref={scrollRef} />
              </div>
            </div>
            <div className="border-t p-4">
              <div className="mx-auto flex max-w-3xl items-end gap-2">
                <Textarea
                  value={draft}
                  onChange={(e) => setDraft(e.target.value)}
                  onKeyDown={handleKeyDown}
                  placeholder="输入消息，Enter 发送，Shift+Enter 换行"
                  rows={2}
                  className="resize-none"
                  disabled={streaming}
                />
                {streaming ? (
                  <Button variant="outline" onClick={stop}>
                    <Square />
                    停止
                  </Button>
                ) : (
                  <Button onClick={handleSend} disabled={!draft.trim()}>
                    <Send />
                    发送
                  </Button>
                )}
              </div>
            </div>
          </>
        )}
      </div>

      <NewConversationDialog
        open={newDialogOpen}
        onOpenChange={setNewDialogOpen}
        onCreated={(id) => {
          setConversationId(id);
          setPending([]);
        }}
      />
    </div>
  );
}

// LLM output goes through marked unsanitized — a prompt-injected reply could
// contain a <script>/onerror payload, so the parsed HTML is run through
// DOMPurify before ever reaching dangerouslySetInnerHTML. breaks:true so a
// single newline renders as a line break, matching how a chat message reads
// (CommonMark's default of needing a blank line between paragraphs feels
// wrong for chat output).
function renderMarkdown(content: string): string {
  const html = marked.parse(content, { breaks: true, gfm: true, async: false });
  return DOMPurify.sanitize(html);
}

function MessageBubble({ message }: { message: Message | PendingMessage }) {
  const isUser = message.role === "user";
  const isPendingAssistant = message.id === "pending-assistant";
  const error = isPendingAssistant ? (message as PendingMessage).error : undefined;
  // "pending-assistant" is the id handleSend seeds before any delta has
  // arrived — empty content at that point means "waiting for the first
  // chunk," not an actual empty reply, so show a loading indicator instead
  // of a blank bubble.
  const isWaitingForFirstChunk = isPendingAssistant && message.content === "" && !error;

  return (
    <div className={cn("flex", isUser ? "justify-end" : "justify-start")}>
      <div
        className={cn(
          "max-w-[80%] rounded-lg px-4 py-2 text-sm",
          isUser ? "whitespace-pre-wrap bg-primary text-primary-foreground" : "bg-muted",
        )}
      >
        {isWaitingForFirstChunk ? (
          <TypingIndicator />
        ) : isUser ? (
          message.content
        ) : (
          <>
            {message.content && (
              <div
                // Only code blocks/inline code get a deliberate style
                // (monospace); everything else here is just enough spacing
                // for paragraphs/lists to not run together — no broader
                // redesign of the bubble itself.
                className="[&_code]:font-mono [&_code]:text-[0.85em] [&_ol]:list-decimal [&_ol]:pl-5 [&_p:last-child]:mb-0 [&_p]:mb-2 [&_pre]:my-2 [&_pre]:overflow-x-auto [&_pre]:rounded-md [&_pre]:bg-background/60 [&_pre]:p-2 [&_ul]:list-disc [&_ul]:pl-5"
                dangerouslySetInnerHTML={{ __html: renderMarkdown(message.content) }}
              />
            )}
            {error && <p className={cn("text-destructive", message.content && "mt-2")}>⚠️ {error}</p>}
          </>
        )}
      </div>
    </div>
  );
}

function TypingIndicator() {
  return (
    <div className="flex items-center gap-1 py-1">
      <span className="size-1.5 animate-bounce rounded-full bg-current [animation-delay:-0.3s]" />
      <span className="size-1.5 animate-bounce rounded-full bg-current [animation-delay:-0.15s]" />
      <span className="size-1.5 animate-bounce rounded-full bg-current" />
    </div>
  );
}
