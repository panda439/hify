import { Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import type { ChatModel } from "@/lib/agents";
import type { KnowledgeBase } from "@/lib/knowledge";
import type { McpTool } from "@/lib/mcp";
import type { WorkflowFlowNode } from "@/routes/workflow-node";

// Every field here writes through onConfigChange(partial) — the editor
// owns the actual node state (see workflow-editor.tsx's updateNodeConfig),
// this component only renders type-specific fields for whichever node is
// currently selected.
export function WorkflowNodePanel({
  node,
  onConfigChange,
  onDelete,
  chatModels,
  knowledgeBases,
  mcpTools,
}: {
  node: WorkflowFlowNode | null;
  onConfigChange: (patch: Record<string, unknown>) => void;
  onDelete: () => void;
  chatModels: ChatModel[];
  knowledgeBases: KnowledgeBase[];
  mcpTools: McpTool[];
}) {
  if (!node) {
    return (
      <div className="flex h-full items-center justify-center p-6 text-center text-sm text-muted-foreground">
        点击画布上的节点查看和编辑配置
      </div>
    );
  }

  const { stepType, config } = node.data;
  const canDelete = stepType !== "start";

  return (
    <div className="flex h-full flex-col gap-4 overflow-y-auto p-4">
      <div className="flex items-center justify-between">
        <p className="text-sm font-medium">节点配置</p>
        {canDelete && (
          <Button variant="ghost" size="icon" onClick={onDelete}>
            <Trash2 className="size-4 text-destructive" />
          </Button>
        )}
      </div>

      {stepType === "start" && <p className="text-sm text-muted-foreground">开始节点没有可配置项，运行时的输入内容会直接作为它的输出传给下一步。</p>}

      {stepType === "end" && (
        <div className="grid gap-2">
          <Label>输出模板（可选）</Label>
          <Textarea
            rows={4}
            placeholder="留空则直接输出上一步的结果，可用 {{.Input}} {{.Prev}} {{.Steps.节点ID}}"
            value={(config.output_template as string) ?? ""}
            onChange={(e) => onConfigChange({ output_template: e.target.value })}
          />
        </div>
      )}

      {stepType === "llm_call" && (
        <>
          <div className="grid gap-2">
            <Label>模型</Label>
            <Select
              value={(config.model_id as string) ?? ""}
              onValueChange={(v) => onConfigChange({ model_id: v })}
            >
              <SelectTrigger>
                <SelectValue placeholder="选择模型" />
              </SelectTrigger>
              <SelectContent>
                {chatModels.map((m) => (
                  <SelectItem key={m.id} value={m.id}>
                    {m.model_name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid gap-2">
            <Label>System Prompt（可选）</Label>
            <Textarea
              rows={2}
              value={(config.system_prompt as string) ?? ""}
              onChange={(e) => onConfigChange({ system_prompt: e.target.value })}
            />
          </div>
          <div className="grid gap-2">
            <Label>提示词模板</Label>
            <Textarea
              rows={4}
              placeholder="可用 {{.Input}} {{.Prev}} {{.Steps.节点ID}} 引用之前的内容"
              value={(config.prompt_template as string) ?? ""}
              onChange={(e) => onConfigChange({ prompt_template: e.target.value })}
            />
          </div>
          <div className="grid grid-cols-2 gap-2">
            <div className="grid gap-2">
              <Label>Temperature</Label>
              <Input
                type="number"
                step="0.1"
                min="0"
                max="2"
                placeholder="0.7"
                value={(config.temperature as number) ?? ""}
                onChange={(e) => onConfigChange({ temperature: e.target.value ? Number(e.target.value) : undefined })}
              />
            </div>
            <div className="grid gap-2">
              <Label>Max Tokens</Label>
              <Input
                type="number"
                min="1"
                placeholder="默认"
                value={(config.max_tokens as number) ?? ""}
                onChange={(e) => onConfigChange({ max_tokens: e.target.value ? Number(e.target.value) : undefined })}
              />
            </div>
          </div>
        </>
      )}

      {stepType === "knowledge_retrieval" && (
        <>
          <div className="grid gap-2">
            <Label>知识库</Label>
            {knowledgeBases.length === 0 ? (
              <p className="text-sm text-muted-foreground">还没有可用的知识库</p>
            ) : (
              <div className="grid max-h-40 gap-2 overflow-y-auto rounded-md border p-2">
                {knowledgeBases.map((kb) => {
                  const selected = Array.isArray(config.knowledge_base_ids)
                    ? (config.knowledge_base_ids as string[])
                    : [];
                  return (
                    <label key={kb.id} className="flex items-center gap-2 text-sm">
                      <Checkbox
                        checked={selected.includes(kb.id)}
                        onCheckedChange={(checked) =>
                          onConfigChange({
                            knowledge_base_ids:
                              checked === true ? [...selected, kb.id] : selected.filter((id) => id !== kb.id),
                          })
                        }
                      />
                      {kb.name}
                    </label>
                  );
                })}
              </div>
            )}
          </div>
          <div className="grid gap-2">
            <Label>检索内容模板</Label>
            <Textarea
              rows={3}
              placeholder="可用 {{.Input}} {{.Prev}} {{.Steps.节点ID}}，如 {{.Input}}"
              value={(config.query_template as string) ?? ""}
              onChange={(e) => onConfigChange({ query_template: e.target.value })}
            />
          </div>
          <div className="grid gap-2">
            <Label>Top K</Label>
            <Input
              type="number"
              min="1"
              placeholder="默认 5"
              value={(config.top_k as number) ?? ""}
              onChange={(e) => onConfigChange({ top_k: e.target.value ? Number(e.target.value) : undefined })}
            />
          </div>
        </>
      )}

      {stepType === "conditional" && (
        <div className="grid gap-2">
          <Label>条件表达式</Label>
          <Textarea
            rows={3}
            placeholder='如 len(input) > 3，可用变量 input / prev / steps.节点ID'
            value={(config.expression as string) ?? ""}
            onChange={(e) => onConfigChange({ expression: e.target.value })}
          />
          <p className="text-xs text-muted-foreground">
            表达式求值结果必须是布尔值，从右侧「真」「假」两个连接点分别连到对应的后续节点。
          </p>
        </div>
      )}

      {stepType === "tool_call" && (
        <>
          <div className="grid gap-2">
            <Label>工具</Label>
            <Select
              value={(config.mcp_tool_id as string) ?? ""}
              onValueChange={(v) => onConfigChange({ mcp_tool_id: v })}
            >
              <SelectTrigger>
                <SelectValue placeholder="选择工具" />
              </SelectTrigger>
              <SelectContent>
                {mcpTools.map((t) => (
                  <SelectItem key={t.id} value={t.id}>
                    {t.tool_name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid gap-2">
            <Label>调用参数模板（JSON）</Label>
            <Textarea
              rows={4}
              placeholder='如 {"city": "{{.Input}}"}'
              value={(config.arguments_template as string) ?? ""}
              onChange={(e) => onConfigChange({ arguments_template: e.target.value })}
            />
          </div>
        </>
      )}
    </div>
  );
}
