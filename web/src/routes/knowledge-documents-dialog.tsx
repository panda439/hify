import { useRef, useState } from "react";
import { Trash2, Upload } from "lucide-react";
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
import { ApiError } from "@/lib/api";
import { useChatModels } from "@/lib/agents";
import { ExtractionPanel } from "@/routes/extraction-panel";
import {
  useDeleteDocument,
  useDocuments,
  useUploadDocument,
  type DocumentStatus,
  type KnowledgeBase,
  type KnowledgeDocument,
} from "@/lib/knowledge";

function statusBadge(status: DocumentStatus) {
  switch (status) {
    case "ready":
      return (
        <Badge className="border-transparent bg-emerald-100 text-emerald-700 dark:bg-emerald-950 dark:text-emerald-400">
          已就绪
        </Badge>
      );
    case "failed":
      return <Badge variant="destructive">处理失败</Badge>;
    case "processing":
      return <Badge variant="secondary">处理中...</Badge>;
    default:
      return <Badge variant="outline">等待处理</Badge>;
  }
}

// --- 007-document-processing-notice：部分页面未能提取文本的提示 ---
//
// ⚠️ 三件**不能**做的事：
//  1. 不得在 status !== "ready" 时展示这个字段。它可能是上一次成功处理留下的，
//     而这一次失败了——那时显示它就是在描述文档已经不在的状态（契约 C5）。
//  2. 不得把它做成 status 的第五种取值。status 的取值集合由数据库 CHECK 固定，
//     且它表达的是**文档可不可用**——带提示的文档是可用的（128 个分片都在，
//     能检索），提示只是可用文档的一个附加说明。
//  3. 不得按缺页数量决定要不要显示。缺 1 页和缺 50 页都要显示——由用户判断
//     重要程度，不是界面替他判断。
//
// 视觉上用 amber 而不是失败那一套（destructive）：这份文档是**可用的**，
// 做得像失败会让用户去删掉重传；但也不能弱到看不见，否则等于没做。

function unextractedCount(d: KnowledgeDocument): number {
  return d.unextracted_pages?.length ?? 0;
}

function unparseableCount(d: KnowledgeDocument): number {
  return d.unparseable_pages?.length ?? 0;
}

// formatPageRanges 把连续页码折叠成区间：[46,47,48,49,50] -> "第 46-50 页"。
// 折叠放在前端而不是后端：后端 DTO 只发事实（原始页码数组），渲染规则改一次
// 不该动 API 契约。扫描件的典型形态就是"连着一段"，不折叠会得到一长串数字。
function formatPageRanges(pages: number[]): string {
  const ranges: string[] = [];
  let start = pages[0];
  let prev = pages[0];
  for (const p of pages.slice(1)) {
    if (p === prev + 1) {
      prev = p;
      continue;
    }
    ranges.push(start === prev ? `${start}` : `${start}-${prev}`);
    start = p;
    prev = p;
  }
  ranges.push(start === prev ? `${start}` : `${start}-${prev}`);
  return `第 ${ranges.join("、")} 页`;
}

// noticeSummary 是列表行里的短版：按需出现的两段，只有一类缺失时就只有一段
// （读起来和 007 那句话一样），两类都有时多一段。数量必须是**真实总数**。
function noticeSummary(d: KnowledgeDocument): string {
  const parts: string[] = [];
  if (unextractedCount(d) > 0) parts.push(`有 ${unextractedCount(d)} 页未提取文本`);
  if (unparseableCount(d) > 0) parts.push(`${unparseableCount(d)} 页无法解析`);
  return parts.join("、");
}

// noticeHint 是悬浮里的完整信息：两段各自给出**不同的**下一步动作。
//
// ⭐ 第二段里的「OCR 对它没有用」是 008 这一整期的落点，**不得删也不得弱化**。
// 用户刚读完第一段的"请用 OCR"，不明说的话他会顺手对这几页也做 OCR，然后发现
// 没用——那时这条提示就从「帮助」变成了「误导」，比不提示更糟。
//
// 两段合并成一句「有 N 页没进去，请检查文件」同样不行：那等于把两列拆开的全部
// 意义扔掉，还不如当初直接塞进一列。
function noticeHint(d: KnowledgeDocument): string {
  const hints: string[] = [];
  const unextracted = d.unextracted_pages ?? [];
  const unparseable = d.unparseable_pages ?? [];
  if (unextracted.length > 0) {
    hints.push(
      `${formatPageRanges(unextracted)}未能提取到文本，通常是扫描图或图片型页面。` +
        `如需检索其中内容，请用 OCR 工具把这些页转换为可选中文字后重新上传。`,
    );
  }
  if (unparseable.length > 0) {
    hints.push(
      `${formatPageRanges(unparseable)}无法解析（页面结构已损坏或不受当前解析器支持），` +
        `OCR 对它没有用；请用其他 PDF 工具重新导出后上传。`,
    );
  }
  return hints.join("\n\n");
}

export function KnowledgeDocumentsDialog({
  open,
  onOpenChange,
  knowledgeBase,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  knowledgeBase: KnowledgeBase | null;
}) {
  const kbId = knowledgeBase?.id ?? "";
  const { data, isLoading } = useDocuments(open ? kbId : null);
  const uploadDocument = useUploadDocument(kbId);
  const deleteDocument = useDeleteDocument(kbId);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const [deletingId, setDeletingId] = useState<string | null>(null);
  // ⚠️ 默认 false：不勾就是这个开关出现之前的行为。
  const [narrative, setNarrative] = useState(false);
  // 010 T035：关系抽取是独立于场景切分的第二个开关。
  const [extractRelations, setExtractRelations] = useState(false);
  const [relationModelId, setRelationModelId] = useState("");
  const { data: chatModels } = useChatModels();

  const documents = data?.items ?? [];

  const handleFileChange = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    e.target.value = ""; // allow re-selecting the same file later
    if (!file) return;
    // ⚠️ 勾了抽取却没选模型时**在这里就挡住**，不发请求。
    // 后端也会拒（两处都挡），但这里挡住的是"用户看到一句能立刻照做的话"。
    if (extractRelations && relationModelId === "") {
      toast.error("开启人物关系抽取需要先选择一个对话模型");
      return;
    }
    try {
      await uploadDocument.mutateAsync({
        file,
        options: { narrative, extractRelations, relationModelId },
      });
      toast.success(
        extractRelations
          ? `${file.name} 已上传，将按场景切分并抽取人物关系`
          : narrative
            ? `${file.name} 已上传，将按场景切分`
            : `${file.name} 已上传，正在处理`,
      );
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "上传失败");
    }
  };

  const handleDelete = async (docId: string) => {
    setDeletingId(docId);
    try {
      await deleteDocument.mutateAsync(docId);
      toast.success("文档已删除");
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "删除失败");
    } finally {
      setDeletingId(null);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>管理文档 — {knowledgeBase?.name}</DialogTitle>
          <DialogDescription>支持 txt / md / pdf，单文件不超过 10MB</DialogDescription>
        </DialogHeader>

        <div className="grid max-h-80 gap-2 overflow-y-auto">
          {isLoading && <p className="text-sm text-muted-foreground">加载中...</p>}
          {!isLoading && documents.length === 0 && (
            <p className="text-sm text-muted-foreground">还没有上传任何文档</p>
          )}
          {documents.map((d) => (
            <div key={d.id} className="rounded-md border p-2">
              <div className="flex items-center justify-between gap-2">
              <div className="min-w-0">
                <div className="flex items-center gap-2">
                  <span className="truncate text-sm font-medium">{d.file_name}</span>
                  {statusBadge(d.status)}
                </div>
                <p className="text-xs text-muted-foreground">
                  {d.status === "ready" ? `${d.chunk_count} 个分片` : d.status === "failed" ? d.error_message : "—"}
                  {d.status === "ready" && noticeSummary(d) !== "" && (
                    <span
                      className="ml-1 text-amber-600 dark:text-amber-500"
                      title={noticeHint(d)}
                    >
                      · {noticeSummary(d)}
                    </span>
                  )}
                </p>
              </div>
              <Button
                variant="ghost"
                size="icon-sm"
                onClick={() => handleDelete(d.id)}
                disabled={deletingId === d.id}
              >
                <Trash2 />
              </Button>
              </div>
              <ExtractionPanel kbId={kbId} doc={d} />
            </div>
          ))}
        </div>

        <div className="border-t pt-4">
          <input
            ref={fileInputRef}
            type="file"
            accept=".txt,.md,.pdf"
            className="hidden"
            onChange={handleFileChange}
          />
          <label className="flex items-start gap-2 text-sm">
            <input
              type="checkbox"
              className="mt-0.5"
              checked={narrative}
              onChange={(e) => setNarrative(e.target.checked)}
              disabled={uploadDocument.isPending}
            />
            <span>
              按场景切分（小说等叙事文本）
              <span className="block text-xs text-muted-foreground">
                按章节标题和场景分隔线切，而不是按长度硬切。仅支持 txt / md，
                上传后不可更改。
              </span>
            </span>
          </label>
          {/* ⭐ 关系抽取只在开了场景切分时才出现：它依赖场景坐标，
              后端也有同样的约束。⚠️ 一直显示、点了才报错，
              等于让用户先做一件注定失败的事。 */}
          {narrative && (
            <label className="mt-2 flex items-start gap-2 text-sm">
              <input
                type="checkbox"
                className="mt-0.5"
                checked={extractRelations}
                onChange={(e) => setExtractRelations(e.target.checked)}
                disabled={uploadDocument.isPending}
              />
              <span className="min-w-0 flex-1">
                同时抽取人物关系
                <span className="block text-xs text-muted-foreground">
                  逐段调用模型抽取人物与关系，会产生模型调用开销，可以随时暂停。
                </span>
                {/* ⚠️ 模型是**必选**，不是"以后再配"：没有模型就没有作业，
                    而文档会带着一个"已开启"的开关停在那里什么都不做。 */}
                {extractRelations && (
                  <select
                    className="mt-1 w-full rounded-md border bg-background px-2 py-1 text-xs"
                    value={relationModelId}
                    onChange={(e) => setRelationModelId(e.target.value)}
                    disabled={uploadDocument.isPending}
                  >
                    <option value="">选择抽取用的对话模型…</option>
                    {(chatModels?.items ?? []).map((m) => (
                      <option key={m.id} value={m.id}>
                        {m.model_name}
                      </option>
                    ))}
                  </select>
                )}
              </span>
            </label>
          )}
          <Button
            className="w-full"
            variant="outline"
            onClick={() => fileInputRef.current?.click()}
            disabled={uploadDocument.isPending}
          >
            <Upload />
            {uploadDocument.isPending ? "上传中..." : "上传文档"}
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}
