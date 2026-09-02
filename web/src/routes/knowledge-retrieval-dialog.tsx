import { useMemo, useState } from "react";
import { Search } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ApiError } from "@/lib/api";
import {
  useDocuments,
  useRetrievalProbe,
  type KnowledgeBase,
  type RetrievalProbeResult,
} from "@/lib/knowledge";

// 后端错误码 -> 面向用户的中文说明。
// knowledge.metadata_filter_disabled 需要额外告诉用户开哪个环境变量——
// 这个开关默认关闭是有意的（静默降级成"不过滤"比报错危险得多），
// 所以这里不能只说"失败了"，得说清楚怎么开。
function errorHint(err: unknown): string {
  if (!(err instanceof ApiError)) return "检索失败，请稍后重试";
  switch (err.code) {
    case "knowledge.metadata_filter_disabled":
      return "检索范围过滤未启用。要使用文档/页码限定，需要在服务端设置环境变量 HIFY_RAG_METADATA_FILTER_ENABLED=true 后重启。不限定范围的检索不受影响。";
    case "knowledge.too_many_filter_documents":
      return "选中的文档太多了（最多 50 份），请缩小范围";
    case "knowledge.invalid_page_range":
      return "页码范围不正确：页码必须是正整数，且起始页不能大于结束页";
    default:
      return err.message || "检索失败，请稍后重试";
  }
}

function ResultList({ result }: { result: RetrievalProbeResult }) {
  return (
    <div className="space-y-2">
      <div className="text-sm text-muted-foreground">
        命中 {result.hit_count} 个片段
        {result.neighbor_count > 0 && <>，另有 {result.neighbor_count} 个邻接块</>}
        {result.filter_applied && <>（已按指定范围过滤）</>}
      </div>

      {result.neighbor_count > 0 && (
        // 不解释的话，"我限定了第 10-15 页，怎么冒出来第 9 页的片段"看起来就是个 bug。
        <p className="rounded-md bg-muted px-3 py-2 text-xs text-muted-foreground">
          邻接块是命中片段的上下文补全（前一块/后一块），用于补上被切块切断的半句话。
          它<strong>不受页码范围约束</strong>，因此可能落在你指定的页码之外；
          但它始终来自你限定的文档之内。
        </p>
      )}

      {result.chunks.map((c) => (
        <div
          key={c.id}
          className={
            c.is_neighbor
              ? "rounded-md border border-dashed bg-muted/40 p-3"
              : "rounded-md border p-3"
          }
        >
          <div className="mb-1 flex flex-wrap items-center gap-2 text-xs">
            {c.is_neighbor ? (
              <Badge variant="outline">邻接块</Badge>
            ) : (
              <Badge variant="secondary">命中</Badge>
            )}
            <span className="font-medium">{c.document_name || c.document_id}</span>
            <span className="text-muted-foreground">
              第 {c.page_number ?? "—"} 页
            </span>
            {!c.is_neighbor && (
              <span className="text-muted-foreground">分数 {c.score.toFixed(3)}</span>
            )}
          </div>
          <p className="whitespace-pre-wrap text-sm">{c.content}</p>
        </div>
      ))}
    </div>
  );
}

export function KnowledgeRetrievalDialog({
  open,
  onOpenChange,
  knowledgeBase,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  knowledgeBase: KnowledgeBase | null;
}) {
  const kbId = knowledgeBase?.id ?? "";
  const { data: docsData, isLoading: docsLoading } = useDocuments(open ? kbId : null);
  const probe = useRetrievalProbe(kbId);

  const [query, setQuery] = useState("");
  const [selected, setSelected] = useState<Record<string, boolean>>({});
  const [pageMin, setPageMin] = useState("");
  const [pageMax, setPageMax] = useState("");
  const [localError, setLocalError] = useState<string | null>(null);

  // 只有已就绪的文档才有 chunk 可供检索——pending/processing/failed 的文档
  // 在库里没有已发布的分片，勾上它等于勾了一个必然无匹配的条件。
  const readyDocs = useMemo(
    () => (docsData?.items ?? []).filter((d) => d.status === "ready"),
    [docsData],
  );

  const selectedIds = Object.keys(selected).filter((id) => selected[id]);

  const reset = () => {
    setQuery("");
    setSelected({});
    setPageMin("");
    setPageMax("");
    setLocalError(null);
    probe.reset();
  };

  // 前端校验只是为了少一次往返，不是安全边界——服务端仍然独立校验。
  const validate = (): string | null => {
    if (query.trim() === "") return "请先输入要检索的问题";
    const min = pageMin.trim() === "" ? null : Number(pageMin);
    const max = pageMax.trim() === "" ? null : Number(pageMax);
    if (min !== null && (!Number.isInteger(min) || min < 1)) return "起始页必须是正整数";
    if (max !== null && (!Number.isInteger(max) || max < 1)) return "结束页必须是正整数";
    if (min !== null && max !== null && min > max) return "起始页不能大于结束页";
    return null;
  };

  const handleSearch = () => {
    const problem = validate();
    setLocalError(problem);
    if (problem) return;

    probe.mutate({
      query: query.trim(),
      top_k: 5,
      document_ids: selectedIds.length > 0 ? selectedIds : undefined,
      page_min: pageMin.trim() === "" ? undefined : Number(pageMin),
      page_max: pageMax.trim() === "" ? undefined : Number(pageMax),
    });
  };

  const result = probe.data;

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) reset();
        onOpenChange(next);
      }}
    >
      <DialogContent className="max-h-[85vh] max-w-3xl overflow-y-auto">
        <DialogHeader>
          <DialogTitle>试检索 · {knowledgeBase?.name}</DialogTitle>
          <DialogDescription>
            这是检索调试工具，用来查看「在指定范围内，这个问题能召回哪些片段」。
            它<strong>不会产生对话</strong>，不调用对话模型，也不会留下任何会话记录。
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="probe-query">问题</Label>
            <Input
              id="probe-query"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") handleSearch();
              }}
              placeholder="例如：部署流程是怎样的"
            />
          </div>

          <div className="space-y-2">
            <Label>限定文档（不选 = 全部）</Label>
            {docsLoading ? (
              <p className="text-sm text-muted-foreground">加载中...</p>
            ) : readyDocs.length === 0 ? (
              <p className="rounded-md border border-dashed px-3 py-2 text-sm text-muted-foreground">
                这个知识库里还没有已就绪的文档。请先上传文档并等待处理完成，否则检索不会有任何结果。
              </p>
            ) : (
              <div className="max-h-40 space-y-1 overflow-y-auto rounded-md border p-2">
                {readyDocs.map((d) => (
                  <label key={d.id} className="flex items-center gap-2 text-sm">
                    <Checkbox
                      checked={!!selected[d.id]}
                      onCheckedChange={(v) =>
                        setSelected((prev) => ({ ...prev, [d.id]: v === true }))
                      }
                    />
                    <span>{d.file_name}</span>
                    <span className="text-xs text-muted-foreground">{d.file_type}</span>
                  </label>
                ))}
              </div>
            )}
          </div>

          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-2">
              <Label htmlFor="probe-page-min">起始页（可留空）</Label>
              <Input
                id="probe-page-min"
                value={pageMin}
                onChange={(e) => setPageMin(e.target.value)}
                inputMode="numeric"
                placeholder="如 10"
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="probe-page-max">结束页（可留空）</Label>
              <Input
                id="probe-page-max"
                value={pageMax}
                onChange={(e) => setPageMax(e.target.value)}
                inputMode="numeric"
                placeholder="如 15"
              />
            </div>
          </div>
          <p className="text-xs text-muted-foreground">
            页码范围只对 PDF 有效。txt / md 文档没有页码，一旦填写页码范围就不会被召回。
          </p>

          <Button onClick={handleSearch} disabled={probe.isPending}>
            <Search className="mr-2 h-4 w-4" />
            {probe.isPending ? "检索中..." : "开始检索"}
          </Button>

          {localError && <p className="text-sm text-destructive">{localError}</p>}
          {probe.isError && <p className="text-sm text-destructive">{errorHint(probe.error)}</p>}

          {result &&
            (result.chunks.length === 0 ? (
              <p className="rounded-md border border-dashed px-3 py-3 text-sm text-muted-foreground">
                {result.filter_applied
                  ? "在你指定的范围内没有召回到任何片段。过滤本身是生效的——可以试着放宽文档或页码范围，确认答案是否真的在这个范围里。"
                  : "没有召回到任何片段。可以换个说法再试，或确认文档是否已经处理完成。"}
              </p>
            ) : (
              <ResultList result={result} />
            ))}
        </div>
      </DialogContent>
    </Dialog>
  );
}
