import { useState } from "react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { ApiError } from "@/lib/api";
import {
  extractionProgress,
  extractionStateLabel,
  extractionStopReasonLabel,
  newIdempotencyKey,
  useExtractionOperation,
  useExtractionStatus,
  type KnowledgeDocument,
} from "@/lib/knowledge";

// extraction-panel.tsx 是一份文档的关系抽取状态与操作（010 T035）。
//
// ⛔ 契约红线：界面上**不出现** epoch / hash / lease / job_id / superseded
// 这类内部术语。用户不需要理解它们，把它们摆出来只会让人以为自己需要理解。
//
// ⭐ 这个面板最要紧的一件事是**把几种"没跑完"分开说**：
//   - 等待中 → 等；
//   - 已暂停 → 去点续跑；
//   - 额度用尽 → 追加额度再续跑；
//   - 已失败 → 看原因，多半要重新处理文档。
// 合并成一个"处理中"会让用户在一个已经停住的作业上一直等下去。

export function ExtractionPanel({
  kbId,
  doc,
}: {
  kbId: string;
  doc: KnowledgeDocument;
}) {
  // ⚠️ 只有开着抽取开关的文档才轮询。存量文档、以及只开了场景切分的文档
  // 一律不发这个请求——它们没有作业，问了也只会得到一个恒定的空状态。
  const enabled = doc.is_relation_extraction_enabled;
  const { data: st } = useExtractionStatus(kbId, doc.id, enabled);
  const op = useExtractionOperation(kbId, doc.id);
  const [busy, setBusy] = useState(false);

  if (!enabled || !st) return null;

  const progress = extractionProgress(st);
  const stopReason = extractionStopReasonLabel(st.stop_reason);

  const run = async (action: "pause" | "resume" | "restart", label: string) => {
    setBusy(true);
    try {
      // ⭐ 幂等键在**点击的这一刻**生成并跟着这次操作走：重试同一次点击用
      // 同一个键，而下一次点击必须是新键。
      await op.mutateAsync({ action, op: { idempotency_key: newIdempotencyKey(action) } });
      toast.success(label);
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : `${label}失败`);
    } finally {
      setBusy(false);
    }
  };

  const live = st.state === "pending" || st.state === "initializing" || st.state === "running";
  const resumable =
    st.state === "paused" || st.state === "budget_exhausted" || st.state === "failed";

  return (
    <div className="mt-1 rounded-md border border-dashed px-2 py-1.5">
      <div className="flex items-center justify-between gap-2">
        <div className="min-w-0 text-xs">
          <span className="font-medium">人物关系抽取：</span>
          <span>{extractionStateLabel(st.state)}</span>
          {/* ⚠️ 进度未知时**什么都不显示**，不写 0/0——那是一个看起来已经
              跑完的进度条，而实际上一条都还没开始。 */}
          {progress && (
            <span className="ml-1 text-muted-foreground">
              {progress[0]} / {progress[1]} 个片段
            </span>
          )}
          {stopReason !== "" && (
            <span className="ml-1 text-amber-600 dark:text-amber-500">· {stopReason}</span>
          )}
          {/* ⭐ 确定发生过的调用次数与"可能发生过"的分开显示。
              ⚠️ 合并会让一个不确定的数字看起来像确定的。 */}
          {st.confirmed_calls !== null && (
            <span className="block text-muted-foreground">
              已发生 {st.confirmed_calls} 次模型调用
              {st.possible_calls !== null && st.possible_calls > st.confirmed_calls && (
                <>（另有 {st.possible_calls - st.confirmed_calls} 次结果未知）</>
              )}
            </span>
          )}
        </div>
        <div className="flex shrink-0 gap-1">
          {live && (
            <Button variant="ghost" size="sm" disabled={busy} onClick={() => run("pause", "已暂停")}>
              暂停
            </Button>
          )}
          {resumable && (
            <Button variant="ghost" size="sm" disabled={busy} onClick={() => run("resume", "已继续")}>
              继续
            </Button>
          )}
          {(resumable || st.state === "succeeded") && (
            <Button
              variant="ghost"
              size="sm"
              disabled={busy}
              onClick={() => run("restart", "已重新开始")}
            >
              重新抽取
            </Button>
          )}
        </div>
      </div>
    </div>
  );
}
