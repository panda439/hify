import { useState } from "react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { ApiError } from "@/lib/api";
import {
  useExtractionAction,
  useExtractionStatus,
  type ExtractionStatus,
} from "@/lib/knowledge";

// knowledge-extraction-panel.tsx 是关系抽取的状态面板（010 T035 的抽取部分）。
//
// ⭐ 这个面板存在的唯一理由是：抽取会**因为各种理由停下来**（额度用尽、
// 连续失败、用户暂停），而停下来的作业没有任何自动重启——按设计如此。
// 没有这个面板，一个暂停的作业只能改数据库才能恢复。
//
// ⚠️ 界面上不出现 epoch、hash、job id 这类内部术语（契约）：它们对用户
// 没有任何可操作性，只会让一个正常的暂停看起来像故障。

const STATE_TEXT: Record<string, string> = {
  initializing: "准备中",
  running: "抽取中",
  paused: "已暂停",
  succeeded: "已完成",
  failed: "已失败",
  superseded: "已被新的一次抽取取代",
};

// 停下来的理由要翻成"用户能据此做决定"的话。
// ⚠️ 特别是预算和连续失败：两者都显示成"已暂停"，但下一步完全不同——
// 前者追加额度就能继续，后者追加额度只会再烧一遍。
const STOP_REASON_TEXT: Record<string, string> = {
  user_paused: "你暂停了它",
  user_disabled: "关系抽取已关闭",
  budget_exhausted: "额度已用尽，续跑前请先追加",
  consecutive_failures: "连续多个片段失败，多半是配置或语料的问题，建议先检查再续跑",
  all_items_failed: "所有片段都失败了",
  completed: "已跑完",
  restarted: "已被新的一次抽取取代",
};

function progressText(st: ExtractionStatus): string {
  // ⚠️ total 未知时不写 0：写 0 的话进度会显示成 "3/0"，
  // 或者更糟——"0/0 已完成"。
  if (st.total_items === null) return `已完成 ${st.succeeded_items} 个片段（总数还在统计中）`;
  const done = st.succeeded_items + st.failed_items;
  return `${done} / ${st.total_items} 个片段` + (st.failed_items > 0 ? `，其中 ${st.failed_items} 个失败` : "");
}

export function ExtractionPanel({ kbId, docId }: { kbId: string; docId: string }) {
  const { data: status, isLoading } = useExtractionStatus(kbId, docId, true);
  const action = useExtractionAction(kbId, docId);
  const [addCalls, setAddCalls] = useState(500);

  const run = async (
    kind: "enable" | "disable" | "pause" | "resume" | "restart",
    body?: Record<string, number | string>,
    okText?: string,
  ) => {
    try {
      await action.mutateAsync({ action: kind, body });
      toast.success(okText ?? "已提交");
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "操作失败");
    }
  };

  if (isLoading) return <p className="text-xs text-muted-foreground">加载抽取状态...</p>;
  if (!status) return null;

  const state = status.state ?? "";
  const busy = action.isPending;

  return (
    <div className="mt-2 rounded-md border bg-muted/40 p-2 text-xs">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-medium">
          关系抽取：{status.enabled ? STATE_TEXT[state] ?? "尚未开始" : "已关闭"}
        </span>
        {status.stop_reason && STOP_REASON_TEXT[status.stop_reason] && (
          <span className="text-muted-foreground">· {STOP_REASON_TEXT[status.stop_reason]}</span>
        )}
      </div>

      {status.job_id && (
        <p className="mt-1 text-muted-foreground">
          {progressText(status)}
          {/* ⚠️ 「不完整」和「空」必须分开说：一次部分失败的抽取给出的关系
              是真的，但**不全**，而用户会拿它当全部。 */}
          {status.has_partial_evidence && (
            <span className="ml-1 text-amber-600 dark:text-amber-500">
              · 结果不完整，失败片段里的关系没有被抽出来
            </span>
          )}
        </p>
      )}

      {status.job_id && (
        <p className="mt-1 text-muted-foreground">
          已用调用 {status.confirmed_calls} 次
          {/* possible 与 confirmed 分开显示：可能发生但结局未知的调用也花了钱。
              合并会让一个不确定的数字看起来像确定的。 */}
          {status.possible_calls > status.confirmed_calls &&
            `（另有 ${status.possible_calls - status.confirmed_calls} 次结果未知）`}
          ，剩余 {status.remaining_calls} 次
          {status.unknown_usage_attempts > 0 &&
            ` · ${status.unknown_usage_attempts} 次调用没能拿到用量数据`}
        </p>
      )}

      <div className="mt-2 flex flex-wrap items-center gap-2">
        {!status.enabled && (
          <Button size="sm" variant="outline" disabled={busy} onClick={() => run("enable", undefined, "已开启")}>
            开启
          </Button>
        )}
        {status.enabled && (state === "running" || state === "initializing") && (
          <Button size="sm" variant="outline" disabled={busy} onClick={() => run("pause", undefined, "已暂停")}>
            暂停
          </Button>
        )}
        {status.enabled && state === "paused" && (
          <>
            <label className="flex items-center gap-1">
              追加调用
              <input
                type="number"
                min={0}
                className="h-7 w-20 rounded border bg-background px-1"
                value={addCalls}
                onChange={(e) => setAddCalls(Number(e.target.value))}
              />
            </label>
            <Button
              size="sm"
              variant="outline"
              disabled={busy}
              onClick={() => run("resume", { additional_calls: addCalls }, "已继续")}
            >
              继续
            </Button>
          </>
        )}
        {status.enabled && (
          <Button size="sm" variant="ghost" disabled={busy} onClick={() => run("disable", undefined, "已关闭")}>
            关闭
          </Button>
        )}
        {/* 重新抽取会丢弃这一次的结果重来（旧账目仍然保留），
            所以放在最后、样式最弱。 */}
        <Button
          size="sm"
          variant="ghost"
          disabled={busy}
          onClick={() => run("restart", undefined, "已重新开始")}
          title="用当前配置重新抽取一次；这一次的结果会被新的一次取代，已花费的调用仍然计入总账"
        >
          重新抽取
        </Button>
      </div>
    </div>
  );
}
