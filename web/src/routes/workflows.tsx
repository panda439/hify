import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { Plus, Workflow as WorkflowIcon } from "lucide-react";
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
import { useUpdateWorkflow, useWorkflows, type Workflow } from "@/lib/workflows";
import { useAuthStore } from "@/stores/auth";
import { WorkflowRunDialog } from "@/routes/workflow-run-dialog";

export function WorkflowsPage() {
  const { data, isLoading } = useWorkflows();
  const updateWorkflow = useUpdateWorkflow();
  const user = useAuthStore((s) => s.user);
  const navigate = useNavigate();
  const [deleting, setDeleting] = useState<Workflow | null>(null);
  const [running, setRunning] = useState<Workflow | null>(null);

  const workflows = data?.items ?? [];
  const canEdit = (wf: Workflow) => user !== null && (wf.created_by === user.id || user.role === "admin");

  // Same soft-delete convention as agent/provider — no DELETE endpoint,
  // is_active=false; the definition itself is resent unchanged since
  // UpdateWorkflow replaces the whole row.
  const handleConfirmDelete = async () => {
    if (!deleting) return;
    try {
      await updateWorkflow.mutateAsync({
        id: deleting.id,
        input: {
          name: deleting.name,
          description: deleting.description,
          definition: deleting.definition,
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
        title="工作流"
        description="用画布编排多步骤流程：模型调用、知识库检索、条件分支、工具调用"
        action={
          <Button onClick={() => navigate("/workflows/new")}>
            <Plus />
            创建工作流
          </Button>
        }
      />

      {!isLoading && workflows.length === 0 && (
        <EmptyState
          icon={<WorkflowIcon className="size-5" />}
          title="还没有创建任何工作流"
          description="用画布编排一个多步骤流程，比如「检索知识库 → 模型总结 → 条件判断」"
          action={
            <Button variant="outline" onClick={() => navigate("/workflows/new")}>
              <Plus />
              创建工作流
            </Button>
          }
        />
      )}

      {workflows.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>名称</TableHead>
              <TableHead>节点数</TableHead>
              <TableHead>状态</TableHead>
              <TableHead className="text-right">操作</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {workflows.map((wf) => (
              <TableRow key={wf.id}>
                <TableCell className="font-medium">
                  {wf.name}
                  {wf.description && (
                    <p className="mt-0.5 truncate text-xs font-normal text-muted-foreground">{wf.description}</p>
                  )}
                </TableCell>
                <TableCell className="text-muted-foreground">{wf.definition.steps.length}</TableCell>
                <TableCell>
                  {wf.is_active ? (
                    <Badge className="border-transparent bg-emerald-100 text-emerald-700 dark:bg-emerald-950 dark:text-emerald-400">
                      启用
                    </Badge>
                  ) : (
                    <Badge variant="outline">已禁用</Badge>
                  )}
                </TableCell>
                <TableCell className="text-right">
                  <div className="flex justify-end gap-2">
                    <Button variant="outline" size="sm" onClick={() => setRunning(wf)} disabled={!wf.is_active}>
                      运行
                    </Button>
                    <Button variant="outline" size="sm" onClick={() => navigate(`/workflows/${wf.id}`)}>
                      {canEdit(wf) ? "编辑" : "查看"}
                    </Button>
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => setDeleting(wf)}
                      disabled={!canEdit(wf)}
                    >
                      删除
                    </Button>
                  </div>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}

      <WorkflowRunDialog open={running !== null} onOpenChange={(open) => !open && setRunning(null)} workflow={running} />

      <AlertDialog open={deleting !== null} onOpenChange={(open) => !open && setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除工作流「{deleting?.name}」？</AlertDialogTitle>
            <AlertDialogDescription>
              删除后该工作流会被禁用，不再能触发新的执行；已有的历史执行记录不受影响，仍可查看。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction variant="destructive" onClick={handleConfirmDelete} disabled={updateWorkflow.isPending}>
              删除
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
