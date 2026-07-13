import { useState } from "react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { ApiError } from "@/lib/api";
import { useAgents } from "@/lib/agents";
import { useCreateConversation } from "@/lib/conversations";

export function NewConversationDialog({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: (conversationId: string) => void;
}) {
  const { data: agentsData } = useAgents();
  const createConversation = useCreateConversation();
  const [agentId, setAgentId] = useState("");

  // Disabled Agents don't affect existing conversations, but new ones may
  // only start against a currently-active Agent — see agent/service.go.
  const agents = (agentsData?.items ?? []).filter((a) => a.is_active);

  const handleCreate = async () => {
    if (!agentId) return;
    try {
      const conv = await createConversation.mutateAsync(agentId);
      onOpenChange(false);
      setAgentId("");
      onCreated(conv.id);
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "创建会话失败");
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>新建对话</DialogTitle>
          <DialogDescription>选择一个 Agent 开始对话</DialogDescription>
        </DialogHeader>

        {agents.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            还没有可用的 Agent，请先到「Agent 管理」创建一个
          </p>
        ) : (
          <Select value={agentId} onValueChange={(v) => setAgentId(v ?? "")}>
            <SelectTrigger>
              <SelectValue placeholder="选择 Agent" />
            </SelectTrigger>
            <SelectContent>
              {agents.map((a) => (
                <SelectItem key={a.id} value={a.id}>
                  {a.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}

        <DialogFooter>
          <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
            取消
          </Button>
          <Button type="button" disabled={!agentId || createConversation.isPending} onClick={handleCreate}>
            开始对话
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
