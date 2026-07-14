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
