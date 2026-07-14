import { useState } from "react";
import { CheckCircle2, CircleDashed, Loader2, XCircle } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";
import { useWorkflowExecution, type Workflow, type WorkflowStepEvent } from "@/lib/workflows";

interface TraceEntry {
  stepId: string;
  stepType: string;
  status: "running" | "succeeded" | "failed";
  output?: string;
  error?: string;
}

// Shared by workflows.tsx (list page "运行" button) and the canvas editor
// (which also passes onStepEvent to live-highlight nodes as they run) —
// the trace/output rendering is identical either way, only the extra
// per-event side effect differs.
export function WorkflowRunDialog({
  open,
  onOpenChange,
  workflow,
  onStepEvent,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  workflow: Workflow | null;
  onStepEvent?: (event: WorkflowStepEvent) => void;
}) {
  const { execute, running } = useWorkflowExecution();
  const [input, setInput] = useState("");
  const [trace, setTrace] = useState<TraceEntry[]>([]);
  const [finalOutput, setFinalOutput] = useState<string | null>(null);
  const [runError, setRunError] = useState<string | null>(null);

  const reset = () => {
    setTrace([]);
    setFinalOutput(null);
    setRunError(null);
  };

  const handleRun = async () => {
    if (!workflow) return;
    reset();

    await execute(workflow.id, input, (event) => {
      onStepEvent?.(event);
      if (event.type === "step" && event.step_id && event.status) {
        setTrace((prev) => {
          if (event.status === "running") {
            return [...prev, { stepId: event.step_id!, stepType: event.step_type ?? "", status: "running" }];
          }
          const idx = prev.map((t) => t.status).lastIndexOf("running");
          if (idx === -1) return prev;
          const updated = [...prev];
          updated[idx] = { ...updated[idx], status: event.status as "succeeded" | "failed", output: event.output, error: event.error };
          return updated;
        });
      } else if (event.type === "done") {
        setFinalOutput(event.output ?? "");
      } else if (event.type === "error") {
        setRunError(event.error ?? "执行失败");
        toast.error(event.error ?? "执行失败");
      }
    });
  };

  return (
    <Dialog open={open} onOpenChange={(o) => { onOpenChange(o); if (!o) reset(); }}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>运行「{workflow?.name}」</DialogTitle>
          <DialogDescription>输入起始内容，实时查看每一步的执行状态</DialogDescription>
        </DialogHeader>

        <Textarea
          placeholder="输入工作流的起始内容（对应 {{.Input}}）"
          rows={3}
          value={input}
          onChange={(e) => setInput(e.target.value)}
          disabled={running}
        />
        <Button onClick={handleRun} disabled={running}>
          {running ? <Loader2 className="animate-spin" /> : null}
          {running ? "执行中..." : "开始运行"}
        </Button>

        {trace.length > 0 && (
          <div className="grid max-h-64 gap-2 overflow-y-auto rounded-md border p-2">
            {trace.map((t, i) => (
              <div key={i} className="flex items-start gap-2 text-sm">
                {t.status === "running" && <Loader2 className="mt-0.5 size-4 shrink-0 animate-spin text-muted-foreground" />}
                {t.status === "succeeded" && <CheckCircle2 className="mt-0.5 size-4 shrink-0 text-emerald-600" />}
                {t.status === "failed" && <XCircle className="mt-0.5 size-4 shrink-0 text-destructive" />}
                <div className="min-w-0">
                  <span className="font-medium">{t.stepId}</span>
                  <span className="text-muted-foreground"> · {t.stepType}</span>
                  {t.output && <p className="mt-0.5 line-clamp-3 whitespace-pre-wrap text-muted-foreground">{t.output}</p>}
                  {t.error && <p className="mt-0.5 text-destructive">{t.error}</p>}
                </div>
              </div>
            ))}
          </div>
        )}

        {finalOutput !== null && (
          <div className="rounded-md border bg-muted/40 p-3 text-sm">
            <div className="mb-1 flex items-center gap-1.5 font-medium text-emerald-700 dark:text-emerald-400">
              <CheckCircle2 className="size-4" />
              执行完成
            </div>
            <p className="whitespace-pre-wrap">{finalOutput}</p>
          </div>
        )}

        {runError !== null && finalOutput === null && (
          <div className="rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm text-destructive">
            {runError}
          </div>
        )}

        {!running && trace.length === 0 && finalOutput === null && runError === null && (
          <p className={cn("flex items-center gap-1.5 text-xs text-muted-foreground")}>
            <CircleDashed className="size-3.5" />
            尚未开始
          </p>
        )}
      </DialogContent>
    </Dialog>
  );
}
