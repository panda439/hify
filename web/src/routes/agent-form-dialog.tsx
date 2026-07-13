import { useEffect } from "react";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
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
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Switch } from "@/components/ui/switch";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { ApiError } from "@/lib/api";
import { useChatModels, useCreateAgent, useUpdateAgent, type Agent } from "@/lib/agents";

const formSchema = z.object({
  name: z.string().min(1, "请输入 Agent 名称"),
  description: z.string().optional(),
  model_id: z.string().min(1, "请选择模型"),
  system_prompt: z.string().optional(),
  temperature: z.number().optional(),
  max_tokens: z.number().optional(),
  top_p: z.number().optional(),
  is_active: z.boolean(),
});

type FormValues = z.infer<typeof formSchema>;

// Empty number inputs must become undefined, not NaN, so the optional zod
// fields actually validate — see model.go's note on this same
// omitted-vs-zero distinction on the backend.
const numberOrUndefined = (v: string) => (v === "" ? undefined : Number(v));

export function AgentFormDialog({
  open,
  onOpenChange,
  agent,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  agent: Agent | null; // null = create mode
}) {
  const isEdit = agent !== null;
  const { data: modelsData, isLoading: modelsLoading } = useChatModels();
  const createAgent = useCreateAgent();
  const updateAgent = useUpdateAgent();
  const models = modelsData?.items ?? [];

  const {
    register,
    handleSubmit,
    watch,
    reset,
    setValue,
    formState: { errors, isSubmitting },
  } = useForm<FormValues>({
    resolver: zodResolver(formSchema),
    defaultValues: {
      name: "",
      description: "",
      model_id: "",
      system_prompt: "",
      temperature: 0.7,
      is_active: true,
    },
  });

  const modelId = watch("model_id");

  useEffect(() => {
    if (open) {
      reset(
        agent
          ? {
              name: agent.name,
              description: agent.description,
              model_id: agent.model_id,
              system_prompt: agent.system_prompt,
              temperature: agent.temperature,
              max_tokens: agent.max_tokens ?? undefined,
              top_p: agent.top_p ?? undefined,
              is_active: agent.is_active,
            }
          : {
              name: "",
              description: "",
              model_id: "",
              system_prompt: "",
              temperature: 0.7,
              is_active: true,
            },
      );
    }
  }, [open, agent, reset]);

  const onSubmit = async (values: FormValues) => {
    try {
      if (isEdit) {
        await updateAgent.mutateAsync({
          id: agent.id,
          input: {
            name: values.name,
            description: values.description,
            model_id: values.model_id,
            system_prompt: values.system_prompt,
            temperature: values.temperature,
            max_tokens: values.max_tokens,
            top_p: values.top_p,
            is_active: values.is_active,
          },
        });
        toast.success("Agent 已更新");
      } else {
        await createAgent.mutateAsync({
          name: values.name,
          description: values.description,
          model_id: values.model_id,
          system_prompt: values.system_prompt,
          temperature: values.temperature,
          max_tokens: values.max_tokens,
          top_p: values.top_p,
        });
        toast.success("Agent 已创建");
      }
      onOpenChange(false);
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "操作失败，请稍后重试");
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{isEdit ? "编辑 Agent" : "创建 Agent"}</DialogTitle>
          <DialogDescription>配置系统提示词、模型和采样参数</DialogDescription>
        </DialogHeader>

        <form className="grid gap-4" onSubmit={handleSubmit(onSubmit)}>
          <div className="grid gap-2">
            <Label htmlFor="name">名称</Label>
            <Input id="name" placeholder="如：客服助手" {...register("name")} />
            {errors.name && <p className="text-sm text-destructive">{errors.name.message}</p>}
          </div>

          <div className="grid gap-2">
            <Label htmlFor="description">描述</Label>
            <Input id="description" placeholder="这个 Agent 用来做什么" {...register("description")} />
          </div>

          <div className="grid gap-2">
            <Label htmlFor="model_id">对话模型</Label>
            {!modelsLoading && models.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                还没有可用的对话模型，请先到「模型管理」页面接入供应商并添加一个 chat 模型
              </p>
            ) : (
              <Select value={modelId} onValueChange={(v) => setValue("model_id", v ?? "", { shouldValidate: true })}>
                <SelectTrigger id="model_id">
                  <SelectValue placeholder="选择模型" />
                </SelectTrigger>
                <SelectContent>
                  {models.map((m) => (
                    <SelectItem key={m.id} value={m.id}>
                      {m.model_name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
            {errors.model_id && <p className="text-sm text-destructive">{errors.model_id.message}</p>}
          </div>

          <div className="grid gap-2">
            <Label htmlFor="system_prompt">System Prompt</Label>
            <Textarea
              id="system_prompt"
              rows={4}
              placeholder="留空则为通用助手"
              {...register("system_prompt")}
            />
          </div>

          <div className="grid grid-cols-3 gap-4">
            <div className="grid gap-2">
              <Label htmlFor="temperature">Temperature</Label>
              <Input
                id="temperature"
                type="number"
                step="0.1"
                min="0"
                max="2"
                {...register("temperature", { setValueAs: numberOrUndefined })}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="top_p">Top P</Label>
              <Input
                id="top_p"
                type="number"
                step="0.1"
                min="0"
                max="1"
                placeholder="默认"
                {...register("top_p", { setValueAs: numberOrUndefined })}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="max_tokens">Max Tokens</Label>
              <Input
                id="max_tokens"
                type="number"
                min="1"
                placeholder="默认"
                {...register("max_tokens", { setValueAs: numberOrUndefined })}
              />
            </div>
          </div>

          {isEdit && (
            <div className="flex items-center justify-between rounded-md border p-3">
              <div>
                <Label htmlFor="is_active">启用</Label>
                <p className="text-sm text-muted-foreground">禁用后不会出现在新建对话的可选列表里</p>
              </div>
              <Switch
                id="is_active"
                checked={watch("is_active")}
                onCheckedChange={(v) => setValue("is_active", v)}
              />
            </div>
          )}

          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              取消
            </Button>
            <Button type="submit" disabled={isSubmitting}>
              {isSubmitting ? "保存中..." : "保存"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
