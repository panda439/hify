import { Handle, Position, type NodeProps, type Node } from "@xyflow/react";
import { Bot, CheckCircle2, Flag, GitBranch, Library, Loader2, Play, Wrench, XCircle } from "lucide-react";
import { cn } from "@/lib/utils";
import type { Step, StepType } from "@/lib/workflows";

export type RunHighlight = "running" | "succeeded" | "failed" | undefined;

export interface WorkflowNodeData extends Record<string, unknown> {
  stepType: StepType;
  config: Record<string, unknown>;
  runStatus?: RunHighlight;
}

export type WorkflowFlowNode = Node<WorkflowNodeData, "workflowStep">;

const STEP_META: Record<StepType, { label: string; icon: typeof Bot; className: string }> = {
  start: { label: "开始", icon: Play, className: "border-slate-300 bg-slate-50 dark:border-slate-600 dark:bg-slate-900" },
  end: { label: "结束", icon: Flag, className: "border-slate-300 bg-slate-50 dark:border-slate-600 dark:bg-slate-900" },
  llm_call: { label: "模型调用", icon: Bot, className: "border-blue-300 bg-blue-50 dark:border-blue-700 dark:bg-blue-950" },
  knowledge_retrieval: {
    label: "知识库检索",
    icon: Library,
    className: "border-purple-300 bg-purple-50 dark:border-purple-700 dark:bg-purple-950",
  },
  conditional: {
    label: "条件分支",
    icon: GitBranch,
    className: "border-amber-300 bg-amber-50 dark:border-amber-700 dark:bg-amber-950",
  },
  tool_call: { label: "工具调用", icon: Wrench, className: "border-emerald-300 bg-emerald-50 dark:border-emerald-700 dark:bg-emerald-950" },
};

// A one-line summary shown under the node's type label — enough to
// recognize which node is which at a glance without opening the config
// panel; never the full config (arguments/prompt templates can be long).
export function summarizeStepConfig(stepType: StepType, config: Record<string, unknown>): string {
  switch (stepType) {
    case "llm_call": {
      const prompt = typeof config.prompt_template === "string" ? config.prompt_template : "";
      return prompt ? `"${prompt.slice(0, 24)}${prompt.length > 24 ? "…" : ""}"` : "未配置提示词";
    }
    case "knowledge_retrieval": {
      const ids = Array.isArray(config.knowledge_base_ids) ? config.knowledge_base_ids : [];
      return ids.length > 0 ? `${ids.length} 个知识库` : "未选择知识库";
    }
    case "conditional": {
      const expr = typeof config.expression === "string" ? config.expression : "";
      return expr || "未配置表达式";
    }
    case "tool_call": {
      return typeof config.mcp_tool_id === "string" && config.mcp_tool_id ? "已选择工具" : "未选择工具";
    }
    case "end": {
      const tmpl = typeof config.output_template === "string" ? config.output_template : "";
      return tmpl ? `输出模板已设置` : "默认输出上一步结果";
    }
    default:
      return "";
  }
}

function statusRing(status: RunHighlight) {
  switch (status) {
    case "running":
      return "ring-2 ring-blue-500";
    case "succeeded":
      return "ring-2 ring-emerald-500";
    case "failed":
      return "ring-2 ring-destructive";
    default:
      return "";
  }
}

export function WorkflowStepNode({ data, selected }: NodeProps<WorkflowFlowNode>) {
  const meta = STEP_META[data.stepType];
  const Icon = meta.icon;

  return (
    <div
      className={cn(
        "w-56 rounded-lg border-2 px-3 py-2 shadow-sm transition-shadow",
        meta.className,
        selected && "shadow-md",
        statusRing(data.runStatus),
      )}
    >
      {data.stepType !== "start" && <Handle type="target" position={Position.Top} />}

      <div className="flex items-center gap-1.5 text-sm font-medium">
        <Icon className="size-4 shrink-0" />
        {meta.label}
        {data.runStatus === "running" && <Loader2 className="ml-auto size-3.5 animate-spin text-blue-600" />}
        {data.runStatus === "succeeded" && <CheckCircle2 className="ml-auto size-3.5 text-emerald-600" />}
        {data.runStatus === "failed" && <XCircle className="ml-auto size-3.5 text-destructive" />}
      </div>
      <p className="mt-0.5 truncate text-xs text-muted-foreground">{summarizeStepConfig(data.stepType, data.config)}</p>

      {data.stepType === "conditional" ? (
        <>
          <Handle type="source" position={Position.Bottom} id="true" style={{ left: "30%" }} className="!bg-emerald-500" />
          <Handle type="source" position={Position.Bottom} id="false" style={{ left: "70%" }} className="!bg-destructive" />
          <div className="mt-1 flex justify-between px-2 text-[10px] text-muted-foreground">
            <span>真</span>
            <span>假</span>
          </div>
        </>
      ) : (
        data.stepType !== "end" && <Handle type="source" position={Position.Bottom} />
      )}
    </div>
  );
}

// Converts a backend Step into this node's data shape — stepById/edge
// derivation lives in workflow-editor.tsx since it needs the full Step
// list at once (edges connect two nodes, not derivable from one Step).
export function stepToNodeData(step: Step): WorkflowNodeData {
  return { stepType: step.type, config: step.config ?? {} };
}
