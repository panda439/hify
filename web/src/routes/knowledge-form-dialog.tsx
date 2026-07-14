import { useEffect } from "react";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { ApiError } from "@/lib/api";
import {
  useCreateKnowledgeBase,
  useEmbeddingModels,
  useUpdateKnowledgeBase,
  type KnowledgeBase,
} from "@/lib/knowledge";

const formSchema = z.object({
  name: z.string().min(1, "请输入知识库名称"),
  description: z.string().optional(),
  embedding_model_id: z.string().min(1, "请选择向量模型"),
  chunk_size: z.number().optional(),
  chunk_overlap: z.number().optional(),
  is_active: z.boolean(),
});

type FormValues = z.infer<typeof formSchema>;

const numberOrUndefined = (v: string) => (v === "" ? undefined : Number(v));

export function KnowledgeFormDialog({
  open,
  onOpenChange,
  knowledgeBase,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  knowledgeBase: KnowledgeBase | null; // null = create mode
}) {
  const isEdit = knowledgeBase !== null;
  const { data: modelsData, isLoading: modelsLoading } = useEmbeddingModels();
  const createKB = useCreateKnowledgeBase();
  const updateKB = useUpdateKnowledgeBase();
  const models = modelsData?.items ?? [];

  const {
    register,
    handleSubmit,
    watch,
    reset,
    setValue,
    formState: { errors, isSubmitting },
  } = useForm<FormValues>({
    resolver: zodResolver(formSchema),
    defaultValues: { name: "", description: "", embedding_model_id: "", is_active: true },
  });

  const embeddingModelId = watch("embedding_model_id");

  useEffect(() => {
    if (open) {
      reset(
        knowledgeBase
          ? {
              name: knowledgeBase.name,
              description: knowledgeBase.description,
              embedding_model_id: knowledgeBase.embedding_model_id,
              chunk_size: knowledgeBase.chunk_size,
              chunk_overlap: knowledgeBase.chunk_overlap,
              is_active: knowledgeBase.is_active,
            }
          : { name: "", description: "", embedding_model_id: "", is_active: true },
      );
    }
  }, [open, knowledgeBase, reset]);

  const onSubmit = async (values: FormValues) => {
    try {
      if (isEdit) {
        await updateKB.mutateAsync({
          id: knowledgeBase.id,
          input: { name: values.name, description: values.description, is_active: values.is_active },
        });
        toast.success("知识库已更新");
      } else {
        await createKB.mutateAsync({
          name: values.name,
          description: values.description,
          embedding_model_id: values.embedding_model_id,
          chunk_size: values.chunk_size,
          chunk_overlap: values.chunk_overlap,
        });
        toast.success("知识库已创建");
      }
      onOpenChange(false);
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "操作失败，请稍后重试");
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{isEdit ? "编辑知识库" : "创建知识库"}</DialogTitle>
          <DialogDescription>
            {isEdit ? "向量模型和分块参数创建后不可修改" : "选择向量模型后不可更改，换模型请新建知识库"}
          </DialogDescription>
        </DialogHeader>

        <form className="grid gap-4" onSubmit={handleSubmit(onSubmit)}>
          <div className="grid gap-2">
            <Label htmlFor="name">名称</Label>
            <Input id="name" placeholder="如：产品文档知识库" {...register("name")} />
            {errors.name && <p className="text-sm text-destructive">{errors.name.message}</p>}
          </div>

          <div className="grid gap-2">
            <Label htmlFor="description">描述</Label>
            <Input id="description" placeholder="这个知识库用来做什么" {...register("description")} />
          </div>

          <div className="grid gap-2">
            <Label htmlFor="embedding_model_id">向量模型</Label>
            {isEdit ? (
              <Input value={knowledgeBase.embedding_model_id} disabled />
            ) : !modelsLoading && models.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                还没有可用的向量模型，请先到「模型管理」页面接入供应商并添加一个 embedding 模型
              </p>
            ) : (
              <Select
                value={embeddingModelId}
                onValueChange={(v) => setValue("embedding_model_id", v ?? "", { shouldValidate: true })}
              >
                <SelectTrigger id="embedding_model_id">
                  <SelectValue placeholder="选择向量模型" />
                </SelectTrigger>
                <SelectContent>
                  {models.map((m) => (
                    <SelectItem key={m.id} value={m.id}>
                      {m.model_name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
            {errors.embedding_model_id && <p className="text-sm text-destructive">{errors.embedding_model_id.message}</p>}
          </div>

          {!isEdit && (
            <div className="grid grid-cols-2 gap-4">
              <div className="grid gap-2">
                <Label htmlFor="chunk_size">分块大小</Label>
                <Input
                  id="chunk_size"
                  type="number"
                  min="1"
                  placeholder="默认 500"
                  {...register("chunk_size", { setValueAs: numberOrUndefined })}
                />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="chunk_overlap">分块重叠</Label>
                <Input
                  id="chunk_overlap"
                  type="number"
                  min="0"
                  placeholder="默认 50"
                  {...register("chunk_overlap", { setValueAs: numberOrUndefined })}
                />
              </div>
            </div>
          )}

          {isEdit && (
            <p className="text-sm text-muted-foreground">
              分块大小 {knowledgeBase.chunk_size} · 分块重叠 {knowledgeBase.chunk_overlap} · 已有{" "}
              {knowledgeBase.total_chunks} / {knowledgeBase.max_chunks} 个分片
            </p>
          )}

          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              取消
            </Button>
            <Button type="submit" disabled={isSubmitting}>
              {isSubmitting ? "保存中..." : "保存"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
