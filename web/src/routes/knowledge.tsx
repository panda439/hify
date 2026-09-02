import { useState } from "react";
import { BookOpen, Plus } from "lucide-react";
import { toast } from "sonner";

import { PageHeader } from "@/components/page-header";
import { EmptyState } from "@/components/empty-state";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { ApiError } from "@/lib/api";
import { useKnowledgeBases, useUpdateKnowledgeBase, type KnowledgeBase } from "@/lib/knowledge";
import { KnowledgeFormDialog } from "@/routes/knowledge-form-dialog";
import { KnowledgeDocumentsDialog } from "@/routes/knowledge-documents-dialog";
import { KnowledgeRetrievalDialog } from "@/routes/knowledge-retrieval-dialog";

export function KnowledgePage() {
  const { data, isLoading } = useKnowledgeBases();
  const updateKB = useUpdateKnowledgeBase();
  const [dialogOpen, setDialogOpen] = useState(false);
  const [editing, setEditing] = useState<KnowledgeBase | null>(null);
  const [documentsOpen, setDocumentsOpen] = useState(false);
  const [managingDocs, setManagingDocs] = useState<KnowledgeBase | null>(null);
  const [deleting, setDeleting] = useState<KnowledgeBase | null>(null);
  const [retrievalOpen, setRetrievalOpen] = useState(false);
  const [probing, setProbing] = useState<KnowledgeBase | null>(null);

  const knowledgeBases = data?.items ?? [];

  const openCreate = () => {
    setEditing(null);
    setDialogOpen(true);
  };

  const openEdit = (kb: KnowledgeBase) => {
    setEditing(kb);
    setDialogOpen(true);
  };

  const openDocuments = (kb: KnowledgeBase) => {
    setManagingDocs(kb);
    setDocumentsOpen(true);
  };

  const openRetrieval = (kb: KnowledgeBase) => {
    setProbing(kb);
    setRetrievalOpen(true);
  };

  // Same soft-delete convention as providers/agents: no DELETE endpoint,
  // "删除" disables the row instead — see knowledge/service.go.
  const handleConfirmDelete = async () => {
    if (!deleting) return;
    try {
      await updateKB.mutateAsync({
        id: deleting.id,
        input: { name: deleting.name, description: deleting.description, is_active: false },
      });
      toast.success(`${deleting.name} 已删除`);
      setDeleting(null);
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "删除失败");
    }
  };

  return (
    <div>
      <PageHeader
        title="知识库"
        description="上传文档，系统自动分块并生成向量，供 Agent 检索使用"
        action={
          <Button onClick={openCreate}>
            <Plus />
            创建知识库
          </Button>
        }
      />

      {!isLoading && knowledgeBases.length === 0 && (
        <EmptyState
          icon={<BookOpen className="size-5" />}
          title="还没有创建任何知识库"
          description="创建知识库、上传文档后，可以在 Agent 表单里关联，让对话检索文档内容"
          action={
            <Button variant="outline" onClick={openCreate}>
              <Plus />
              创建知识库
            </Button>
          }
        />
      )}

      {knowledgeBases.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>名称</TableHead>
              <TableHead>分片数</TableHead>
              <TableHead>状态</TableHead>
              <TableHead className="text-right">操作</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {knowledgeBases.map((kb) => (
              <TableRow key={kb.id}>
                <TableCell className="font-medium">
                  {kb.name}
                  {kb.description && (
                    <p className="mt-0.5 truncate text-xs font-normal text-muted-foreground">{kb.description}</p>
                  )}
                </TableCell>
                <TableCell className="text-muted-foreground">
                  {kb.total_chunks} / {kb.max_chunks}
                </TableCell>
                <TableCell>
                  {kb.is_active ? (
                    <Badge className="border-transparent bg-emerald-100 text-emerald-700 dark:bg-emerald-950 dark:text-emerald-400">
                      启用
                    </Badge>
                  ) : (
                    <Badge variant="outline">已禁用</Badge>
                  )}
                </TableCell>
                <TableCell className="text-right">
                  <div className="flex justify-end gap-2">
                    <Button variant="outline" size="sm" onClick={() => openDocuments(kb)}>
                      管理文档
                    </Button>
                    <Button variant="outline" size="sm" onClick={() => openRetrieval(kb)}>
                      试检索
                    </Button>
                    <Button variant="outline" size="sm" onClick={() => openEdit(kb)}>
                      编辑
                    </Button>
                    <Button variant="outline" size="sm" onClick={() => setDeleting(kb)}>
                      删除
                    </Button>
                  </div>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}

      <KnowledgeFormDialog open={dialogOpen} onOpenChange={setDialogOpen} knowledgeBase={editing} />
      <KnowledgeDocumentsDialog open={documentsOpen} onOpenChange={setDocumentsOpen} knowledgeBase={managingDocs} />
      <KnowledgeRetrievalDialog open={retrievalOpen} onOpenChange={setRetrievalOpen} knowledgeBase={probing} />

      <AlertDialog open={deleting !== null} onOpenChange={(open) => !open && setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除知识库「{deleting?.name}」？</AlertDialogTitle>
            <AlertDialogDescription>
              删除后该知识库会被禁用，检索时不再使用；已关联它的 Agent 不受影响，历史对话不受影响。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction variant="destructive" onClick={handleConfirmDelete} disabled={updateKB.isPending}>
              删除
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
