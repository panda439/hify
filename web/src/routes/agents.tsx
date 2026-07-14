import { useState } from "react";
import { Bot, Plus } from "lucide-react";
import { toast } from "sonner";

import { PageHeader } from "@/components/page-header";
import { EmptyState } from "@/components/empty-state";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { ApiError } from "@/lib/api";
import { useAgents, useChatModels, useUpdateAgent, type Agent } from "@/lib/agents";
import { useAuthStore } from "@/stores/auth";
import { AgentFormDialog } from "@/routes/agent-form-dialog";

export function AgentsPage() {
  const { data, isLoading } = useAgents();
  const { data: modelsData } = useChatModels();
  const updateAgent = useUpdateAgent();
  const user = useAuthStore((s) => s.user);
  const [dialogOpen, setDialogOpen] = useState(false);
  const [editing, setEditing] = useState<Agent | null>(null);
  const [deleting, setDeleting] = useState<Agent | null>(null);

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

  // Same soft-delete convention as providers: no DELETE endpoint, agents
  // are a main-data table (is_active), and conversations that already
  // reference this Agent keep working — see agent/service.go's UpdateAgent.
  const handleConfirmDelete = async () => {
    if (!deleting) return;
    try {
      await updateAgent.mutateAsync({
        id: deleting.id,
        input: {
          name: deleting.name,
          description: deleting.description,
          model_id: deleting.model_id,
          system_prompt: deleting.system_prompt,
          temperature: deleting.temperature,
          max_tokens: deleting.max_tokens ?? undefined,
          top_p: deleting.top_p ?? undefined,
          is_active: false,
        },
      });
      toast.success(`${deleting.name} 已删除`);
      setDeleting(null);
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "删除失败");
    }
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
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>名称</TableHead>
              <TableHead>模型</TableHead>
              <TableHead>状态</TableHead>
              <TableHead className="text-right">操作</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {agents.map((a) => (
              <TableRow key={a.id}>
                <TableCell className="font-medium">
                  {a.name}
                  {a.description && (
                    <p className="mt-0.5 truncate text-xs font-normal text-muted-foreground">{a.description}</p>
                  )}
                </TableCell>
                <TableCell className="text-muted-foreground">
                  {modelNameById.get(a.model_id) ?? a.model_id}
                </TableCell>
                <TableCell>
                  {a.is_active ? (
                    <Badge className="border-transparent bg-emerald-100 text-emerald-700 dark:bg-emerald-950 dark:text-emerald-400">
                      启用
                    </Badge>
                  ) : (
                    <Badge variant="outline">已禁用</Badge>
                  )}
                </TableCell>
                <TableCell className="text-right">
                  <div className="flex justify-end gap-2">
                    <Button variant="outline" size="sm" onClick={() => openEdit(a)} disabled={!canEdit(a)}>
                      编辑
                    </Button>
                    <Button variant="outline" size="sm" onClick={() => setDeleting(a)} disabled={!canEdit(a)}>
                      删除
                    </Button>
                  </div>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}

      <AgentFormDialog open={dialogOpen} onOpenChange={setDialogOpen} agent={editing} />

      <AlertDialog open={deleting !== null} onOpenChange={(open) => !open && setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除 Agent「{deleting?.name}」？</AlertDialogTitle>
            <AlertDialogDescription>
              删除后该 Agent 会被禁用，不再出现在新建对话的可选列表里；已有的历史对话不受影响，仍可查看。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction variant="destructive" onClick={handleConfirmDelete} disabled={updateAgent.isPending}>
              删除
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
