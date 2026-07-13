import { useEffect, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { MessageSquare, Plus, Send, Square } from "lucide-react";
import { toast } from "sonner";

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
interface PendingMessage {
  id: string;
  role: "user" | "assistant";
  content: string;
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

    await send(conversationId, content, (event) => {
      if (event.type === "delta") {
        setPending((prev) =>
          prev.map((m) => (m.id === "pending-assistant" ? { ...m, content: m.content + (event.content ?? "") } : m)),
        );
      } else if (event.type === "done") {
        setPending([]);
        qc.invalidateQueries({ queryKey: messagesQueryKey(conversationId) });
        qc.invalidateQueries({ queryKey: conversationsQueryKey() });
      } else if (event.type === "error") {
        toast.error(event.error ?? "对话出错，请重试");
        setPending([]);
        qc.invalidateQueries({ queryKey: messagesQueryKey(conversationId) });
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
      <div className="flex w-72 shrink-0 flex-col border-r bg-muted/30 p-3">
        <Button className="mb-3 justify-start" variant="outline" onClick={() => setNewDialogOpen(true)}>
          <Plus />
          新建对话
        </Button>
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
                <div className="truncate">{c.title || agentNameById.get(c.agent_id) || "对话"}</div>
              </button>
            ))}
          </div>
        )}
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

function MessageBubble({ message }: { message: Message | PendingMessage }) {
  const isUser = message.role === "user";
  return (
    <div className={cn("flex", isUser ? "justify-end" : "justify-start")}>
      <div
        className={cn(
          "max-w-[80%] whitespace-pre-wrap rounded-lg px-4 py-2 text-sm",
          isUser ? "bg-primary text-primary-foreground" : "bg-muted",
        )}
      >
        {message.content || (!isUser && "​")}
      </div>
    </div>
  );
}
