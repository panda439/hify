import { useState } from "react";
import { Plug, Plus, RefreshCw } from "lucide-react";
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
import { useProviders, useTestConnection, useUpdateProvider, type Provider } from "@/lib/providers";
import { ProviderFormDialog } from "@/routes/provider-form-dialog";
import { ProviderModelsDialog } from "@/routes/provider-models-dialog";

function statusBadge(provider: Provider) {
  if (!provider.is_active) return <Badge variant="outline">已禁用</Badge>;
  switch (provider.last_test_status) {
    case "success":
      return (
        <Badge className="border-transparent bg-emerald-100 text-emerald-700 dark:bg-emerald-950 dark:text-emerald-400">
          连接正常
        </Badge>
      );
    case "failed":
      return <Badge variant="destructive">连接失败</Badge>;
    default:
      return <Badge variant="secondary">尚未测试</Badge>;
  }
}

export function ModelsPage() {
  const { data, isLoading } = useProviders();
  const testConnection = useTestConnection();
  const updateProvider = useUpdateProvider();
  const [dialogOpen, setDialogOpen] = useState(false);
  const [editing, setEditing] = useState<Provider | null>(null);
  const [testingId, setTestingId] = useState<string | null>(null);
  const [modelsDialogOpen, setModelsDialogOpen] = useState(false);
  const [managingModels, setManagingModels] = useState<Provider | null>(null);
  const [deleting, setDeleting] = useState<Provider | null>(null);

  const providers = data?.items ?? [];

  const openCreate = () => {
    setEditing(null);
    setDialogOpen(true);
  };

  const openEdit = (p: Provider) => {
    setEditing(p);
    setDialogOpen(true);
  };

  const openModels = (p: Provider) => {
    setManagingModels(p);
    setModelsDialogOpen(true);
  };

  const handleTest = async (p: Provider) => {
    setTestingId(p.id);
    try {
      await testConnection.mutateAsync(p.id);
      toast.success(`${p.name} 连接测试成功`);
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "连接测试失败");
    } finally {
      setTestingId(null);
    }
  };

  // There's no hard-delete endpoint — model_providers is a main-data table
  // and uses the project-wide soft-delete convention (is_active), same as
  // agents. "删除" in the UI maps to disabling, not a DB row deletion, so
  // provider_models/agents that still reference this row don't end up
  // pointing at nothing.
  const handleConfirmDelete = async () => {
    if (!deleting) return;
    try {
      await updateProvider.mutateAsync({
        id: deleting.id,
        input: {
          name: deleting.name,
          base_url: deleting.base_url,
          auth_type: deleting.auth_type,
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
        title="模型管理"
        description="接入 OpenAI 兼容协议的模型供应商，供 Agent 和知识库调用"
        action={
          <Button onClick={openCreate}>
            <Plus />
            添加供应商
          </Button>
        }
      />

      {!isLoading && providers.length === 0 && (
        <EmptyState
          icon={<Plug className="size-5" />}
          title="还没有配置任何模型供应商"
          description="添加一个供应商（如 OpenAI、DeepSeek、Ollama）后，才能创建 Agent 或知识库"
          action={
            <Button variant="outline" onClick={openCreate}>
              <Plus />
              添加供应商
            </Button>
          }
        />
      )}

      {providers.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>名称</TableHead>
              <TableHead>Base URL</TableHead>
              <TableHead>状态</TableHead>
              <TableHead className="text-right">操作</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {providers.map((p) => (
              <TableRow key={p.id}>
                <TableCell className="font-medium">{p.name}</TableCell>
                <TableCell className="max-w-xs truncate text-muted-foreground">{p.base_url}</TableCell>
                <TableCell>{statusBadge(p)}</TableCell>
                <TableCell className="text-right">
                  <div className="flex justify-end gap-2">
                    <Button variant="outline" size="sm" onClick={() => handleTest(p)} disabled={testingId === p.id}>
                      <RefreshCw className={testingId === p.id ? "animate-spin" : ""} />
                      测试连接
                    </Button>
                    <Button variant="outline" size="sm" onClick={() => openModels(p)}>
                      管理模型
                    </Button>
                    <Button variant="outline" size="sm" onClick={() => openEdit(p)}>
                      编辑
                    </Button>
                    <Button variant="outline" size="sm" onClick={() => setDeleting(p)}>
                      删除
                    </Button>
                  </div>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}

      <ProviderFormDialog open={dialogOpen} onOpenChange={setDialogOpen} provider={editing} />
      <ProviderModelsDialog open={modelsDialogOpen} onOpenChange={setModelsDialogOpen} provider={managingModels} />

      <AlertDialog open={deleting !== null} onOpenChange={(open) => !open && setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除供应商「{deleting?.name}」？</AlertDialogTitle>
            <AlertDialogDescription>
              删除后该供应商会被禁用，不再出现在可选列表里；已经引用它的 Agent 不受影响，历史数据不会丢失。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction variant="destructive" onClick={handleConfirmDelete} disabled={updateProvider.isPending}>
              删除
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
