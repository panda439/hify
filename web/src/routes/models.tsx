import { useState } from "react";
import { Plug, Plus, RefreshCw } from "lucide-react";
import { toast } from "sonner";

import { PageHeader } from "@/components/page-header";
import { EmptyState } from "@/components/empty-state";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { ApiError } from "@/lib/api";
import { useProviders, useTestConnection, type Provider } from "@/lib/providers";
import { ProviderFormDialog } from "@/routes/provider-form-dialog";
import { ProviderModelsDialog } from "@/routes/provider-models-dialog";

function statusBadge(provider: Provider) {
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
  const [dialogOpen, setDialogOpen] = useState(false);
  const [editing, setEditing] = useState<Provider | null>(null);
  const [testingId, setTestingId] = useState<string | null>(null);
  const [modelsDialogOpen, setModelsDialogOpen] = useState(false);
  const [managingModels, setManagingModels] = useState<Provider | null>(null);

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
        <div className="grid gap-3">
          {providers.map((p) => (
            <Card key={p.id}>
              <CardContent className="flex items-center justify-between gap-4 py-4">
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <span className="font-medium">{p.name}</span>
                    {statusBadge(p)}
                    {!p.is_active && <Badge variant="outline">已禁用</Badge>}
                  </div>
                  <p className="mt-1 truncate text-sm text-muted-foreground">{p.base_url}</p>
                  {p.last_test_status === "failed" && p.last_test_error && (
                    <p className="mt-1 text-sm text-destructive">{p.last_test_error}</p>
                  )}
                </div>
                <div className="flex shrink-0 items-center gap-2">
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
                </div>
              </CardContent>
            </Card>
          ))}
        </div>
      )}

      <ProviderFormDialog open={dialogOpen} onOpenChange={setDialogOpen} provider={editing} />
      <ProviderModelsDialog open={modelsDialogOpen} onOpenChange={setModelsDialogOpen} provider={managingModels} />
    </div>
  );
}
