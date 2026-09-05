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

// unextractedHint 是悬浮上的完整信息。列表那一行只放短版（含**真实总数**），
// 因为把整句塞进一行要么撑爆布局、要么被 CSS 截断成一句残句，那比不显示更糟。
// 短版负责让用户注意到，这里负责让他知道下一步做什么。
function unextractedHint(d: KnowledgeDocument): string {
  const pages = d.unextracted_pages ?? [];
  if (pages.length === 0) return "";
  return (
    `${formatPageRanges(pages)}未能提取到文本，通常是扫描图或图片型页面，` +
    `这部分内容没有进入知识库、也检索不到。` +
    `如需检索其中内容，请用 OCR 工具把这些页转换为可选中文字后重新上传。`
  );
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

  const documents = data?.items ?? [];

  const handleFileChange = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    e.target.value = ""; // allow re-selecting the same file later
    if (!file) return;
    try {
      await uploadDocument.mutateAsync(file);
      toast.success(`${file.name} 已上传，正在处理`);
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
            <div key={d.id} className="flex items-center justify-between gap-2 rounded-md border p-2">
              <div className="min-w-0">
                <div className="flex items-center gap-2">
                  <span className="truncate text-sm font-medium">{d.file_name}</span>
                  {statusBadge(d.status)}
                </div>
                <p className="text-xs text-muted-foreground">
                  {d.status === "ready" ? `${d.chunk_count} 个分片` : d.status === "failed" ? d.error_message : "—"}
                  {d.status === "ready" && unextractedCount(d) > 0 && (
                    <span
                      className="ml-1 text-amber-600 dark:text-amber-500"
                      title={unextractedHint(d)}
                    >
                      · 有 {unextractedCount(d)} 页未提取文本
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
