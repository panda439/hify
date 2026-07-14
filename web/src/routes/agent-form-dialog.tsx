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
import { Checkbox } from "@/components/ui/checkbox";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { ApiError } from "@/lib/api";
import { useChatModels, useCreateAgent, useUpdateAgent, type Agent } from "@/lib/agents";
import { useKnowledgeBases } from "@/lib/knowledge";
import { useActiveMcpTools } from "@/lib/mcp";

const formSchema = z.object({
  name: z.string().min(1, "请输入 Agent 名称"),
  description: z.string().optional(),
  model_id: z.string().min(1, "请选择模型"),
  system_prompt: z.string().optional(),
  temperature: z.number().optional(),
  max_tokens: z.number().optional(),
  top_p: z.number().optional(),
  knowledge_base_ids: z.array(z.string()),
  mcp_tool_ids: z.array(z.string()),
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
  const { data: kbData } = useKnowledgeBases();
  const { data: mcpToolsData } = useActiveMcpTools();
  const createAgent = useCreateAgent();
  const updateAgent = useUpdateAgent();
  const models = modelsData?.items ?? [];
  // Only active knowledge bases are offered — a disabled one is already
  // skipped at retrieval time (see knowledge/service.go), so listing it
  // here would just be a confusing dead option.
  const knowledgeBases = (kbData?.items ?? []).filter((kb) => kb.is_active);
  // /mcp-tools already only returns tools from active servers that are
  // themselves active (see mcp.Service.ListActiveTools) — no extra filter
  // needed here, unlike knowledge bases above.
  const mcpTools = mcpToolsData?.items ?? [];

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
      knowledge_base_ids: [],
      mcp_tool_ids: [],
      is_active: true,
    },
  });

  const modelId = watch("model_id");
  const selectedKnowledgeBaseIds = watch("knowledge_base_ids");
  const selectedMcpToolIds = watch("mcp_tool_ids");

  const toggleKnowledgeBase = (kbId: string, checked: boolean) => {
    const current = selectedKnowledgeBaseIds;
    setValue(
      "knowledge_base_ids",
      checked ? [...current, kbId] : current.filter((id) => id !== kbId),
    );
  };

  const toggleMcpTool = (toolId: string, checked: boolean) => {
    const current = selectedMcpToolIds;
    setValue(
      "mcp_tool_ids",
      checked ? [...current, toolId] : current.filter((id) => id !== toolId),
    );
  };

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
              knowledge_base_ids: agent.knowledge_base_ids ?? [],
              mcp_tool_ids: agent.mcp_tool_ids ?? [],
              is_active: agent.is_active,
            }
          : {
              name: "",
              description: "",
              model_id: "",
              system_prompt: "",
              temperature: 0.7,
              knowledge_base_ids: [],
              mcp_tool_ids: [],
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
            knowledge_base_ids: values.knowledge_base_ids,
            mcp_tool_ids: values.mcp_tool_ids,
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
          knowledge_base_ids: values.knowledge_base_ids,
          mcp_tool_ids: values.mcp_tool_ids,
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

          <div className="grid gap-2">
            <Label>关联知识库</Label>
            {knowledgeBases.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                还没有可用的知识库，去「知识库」页面创建一个后可以在这里勾选
              </p>
            ) : (
              <div className="grid max-h-40 gap-2 overflow-y-auto rounded-md border p-2">
                {knowledgeBases.map((kb) => (
                  <label key={kb.id} className="flex items-center gap-2 text-sm">
                    <Checkbox
                      checked={selectedKnowledgeBaseIds.includes(kb.id)}
                      onCheckedChange={(checked) => toggleKnowledgeBase(kb.id, checked === true)}
                    />
                    {kb.name}
                  </label>
                ))}
              </div>
            )}
            <p className="text-xs text-muted-foreground">对话时会检索勾选的知识库，把相关内容作为参考资料注入上下文</p>
          </div>

          <div className="grid gap-2">
            <Label>关联 MCP 工具</Label>
            {mcpTools.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                还没有可用的工具，去「MCP 工具」页面添加服务器并同步后可以在这里勾选
              </p>
            ) : (
              <div className="grid max-h-40 gap-2 overflow-y-auto rounded-md border p-2">
                {mcpTools.map((tool) => (
                  <label key={tool.id} className="flex items-center gap-2 text-sm">
                    <Checkbox
                      checked={selectedMcpToolIds.includes(tool.id)}
                      onCheckedChange={(checked) => toggleMcpTool(tool.id, checked === true)}
                    />
                    {tool.tool_name}
                    {tool.description && <span className="text-xs text-muted-foreground">— {tool.description}</span>}
                  </label>
                ))}
              </div>
            )}
            <p className="text-xs text-muted-foreground">对话中模型可以自主决定是否调用勾选的工具</p>
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
