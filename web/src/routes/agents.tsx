import { useState } from "react";
import { Bot, Plus } from "lucide-react";

import { PageHeader } from "@/components/page-header";
import { EmptyState } from "@/components/empty-state";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { useAgents, useChatModels, type Agent } from "@/lib/agents";
import { useAuthStore } from "@/stores/auth";
import { AgentFormDialog } from "@/routes/agent-form-dialog";

export function AgentsPage() {
  const { data, isLoading } = useAgents();
  const { data: modelsData } = useChatModels();
  const user = useAuthStore((s) => s.user);
  const [dialogOpen, setDialogOpen] = useState(false);
  const [editing, setEditing] = useState<Agent | null>(null);

  const agents = data?.items ?? [];
  const modelNameById = new Map((modelsData?.items ?? []).map((m) => [m.id, m.model_name]));

  const canEdit = (a: Agent) => user !== null && (a.created_by === user.id || user.role === "admin");

  const openCreate = () => {
    setEditing(null);
    setDialogOpen(true);
  };

  const openEdit = (a: Agent) => {
    setEditing(a);
    setDialogOpen(true);
  };

  return (
    <div>
      <PageHeader
        title="Agent 管理"
        description="配置系统提示词、模型和参数，创建可对话的 Agent"
        action={
          <Button onClick={openCreate}>
            <Plus />
            创建 Agent
          </Button>
        }
      />

      {!isLoading && agents.length === 0 && (
        <EmptyState
          icon={<Bot className="size-5" />}
          title="还没有创建任何 Agent"
          description="创建一个 Agent 并关联模型供应商后，就可以在「对话」里开始聊天"
          action={
            <Button variant="outline" onClick={openCreate}>
              <Plus />
              创建 Agent
            </Button>
          }
        />
      )}

      {agents.length > 0 && (
        <div className="grid gap-3">
          {agents.map((a) => (
            <Card key={a.id}>
              <CardContent className="flex items-center justify-between gap-4 py-4">
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <span className="font-medium">{a.name}</span>
                    <Badge variant="secondary">{modelNameById.get(a.model_id) ?? a.model_id}</Badge>
                    {!a.is_active && <Badge variant="outline">已禁用</Badge>}
                  </div>
                  {a.description && (
                    <p className="mt-1 truncate text-sm text-muted-foreground">{a.description}</p>
                  )}
                </div>
                <div className="flex shrink-0 items-center gap-2">
                  <Button variant="outline" size="sm" onClick={() => openEdit(a)} disabled={!canEdit(a)}>
                    编辑
                  </Button>
                </div>
              </CardContent>
            </Card>
          ))}
        </div>
      )}

      <AgentFormDialog open={dialogOpen} onOpenChange={setDialogOpen} agent={editing} />
    </div>
  );
}
