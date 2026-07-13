import { useState } from "react";
import { Plus } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { ApiError } from "@/lib/api";
import {
  useAddModel,
  useProviderModels,
  useUpdateModel,
  type Provider,
  type ProviderModel,
} from "@/lib/providers";

export function ProviderModelsDialog({
  open,
  onOpenChange,
  provider,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  provider: Provider | null;
}) {
  const providerId = provider?.id ?? "";
  const { data, isLoading } = useProviderModels(open ? providerId : null);
  const addModel = useAddModel(providerId);
  const updateModel = useUpdateModel(providerId);

  const [modelName, setModelName] = useState("");
  const [capability, setCapability] = useState<"chat" | "embedding">("chat");
  const [contextWindow, setContextWindow] = useState("");
  const [embeddingDimension, setEmbeddingDimension] = useState("");

  const models = data?.items ?? [];

  const handleAdd = async () => {
    if (!modelName.trim()) {
      toast.error("请输入模型标识符");
      return;
    }
    try {
      await addModel.mutateAsync({
        model_name: modelName.trim(),
        capability,
        context_window: contextWindow ? Number(contextWindow) : undefined,
        embedding_dimension: embeddingDimension ? Number(embeddingDimension) : undefined,
      });
      toast.success("模型已添加");
      setModelName("");
      setContextWindow("");
      setEmbeddingDimension("");
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "添加失败");
    }
  };

  // Backend replaces the whole row on update — always pass through the
  // model's current fields, only flipping the one being toggled.
  const patchModel = async (m: ProviderModel, patch: Partial<{ isDefault: boolean; isActive: boolean }>) => {
    try {
      await updateModel.mutateAsync({
        id: m.id,
        input: {
          context_window: m.context_window ?? undefined,
          max_output_tokens: m.max_output_tokens ?? undefined,
          embedding_dimension: m.embedding_dimension ?? undefined,
          is_default: patch.isDefault ?? m.is_default,
          is_active: patch.isActive ?? m.is_active,
        },
      });
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "更新失败");
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>管理模型 — {provider?.name}</DialogTitle>
          <DialogDescription>一个供应商下可以有多个对话/向量模型</DialogDescription>
        </DialogHeader>

        <div className="grid max-h-64 gap-2 overflow-y-auto">
          {isLoading && <p className="text-sm text-muted-foreground">加载中...</p>}
          {!isLoading && models.length === 0 && (
            <p className="text-sm text-muted-foreground">还没有添加任何模型</p>
          )}
          {models.map((m) => (
            <div key={m.id} className="flex items-center justify-between gap-2 rounded-md border p-2">
              <div className="min-w-0">
                <div className="flex items-center gap-2">
                  <span className="truncate text-sm font-medium">{m.model_name}</span>
                  <Badge variant="secondary">{m.capability === "chat" ? "对话" : "向量"}</Badge>
                  {m.is_default && <Badge>默认</Badge>}
                </div>
                <p className="text-xs text-muted-foreground">
                  {m.capability === "chat"
                    ? m.context_window
                      ? `上下文窗口 ${m.context_window}`
                      : "未设置上下文窗口"
                    : m.embedding_dimension
                      ? `维度 ${m.embedding_dimension}`
                      : "未设置维度"}
                </p>
              </div>
              <div className="flex shrink-0 items-center gap-2">
                {!m.is_default && (
                  <Button variant="ghost" size="sm" onClick={() => patchModel(m, { isDefault: true })}>
                    设为默认
                  </Button>
                )}
                <Switch checked={m.is_active} onCheckedChange={(checked) => patchModel(m, { isActive: checked })} />
              </div>
            </div>
          ))}
        </div>

        <div className="grid gap-2 border-t pt-4">
          <Label>添加模型</Label>
          <div className="grid grid-cols-2 gap-2">
            <Input
              placeholder="模型标识符，如 gpt-4o"
              value={modelName}
              onChange={(e) => setModelName(e.target.value)}
            />
            <Select value={capability} onValueChange={(v) => setCapability(v as "chat" | "embedding")}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="chat">对话模型</SelectItem>
                <SelectItem value="embedding">向量模型</SelectItem>
              </SelectContent>
            </Select>
          </div>
          {capability === "chat" ? (
            <Input
              placeholder="上下文窗口（可选，如 128000）"
              type="number"
              value={contextWindow}
              onChange={(e) => setContextWindow(e.target.value)}
            />
          ) : (
            <Input
              placeholder="向量维度（可选，如 1536）"
              type="number"
              value={embeddingDimension}
              onChange={(e) => setEmbeddingDimension(e.target.value)}
            />
          )}
          <Button onClick={handleAdd} disabled={addModel.isPending}>
            <Plus />
            添加
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}
