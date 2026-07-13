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
import { useCreateProvider, useUpdateProvider, type Provider } from "@/lib/providers";

const formSchema = z.object({
  name: z.string().min(1, "请输入供应商名称"),
  base_url: z.string().min(1, "请输入 Base URL").url("请输入合法的 URL，如 https://api.deepseek.com/v1"),
  auth_type: z.enum(["api_key", "none"]),
  api_key: z.string().optional(),
});

type FormValues = z.infer<typeof formSchema>;

export function ProviderFormDialog({
  open,
  onOpenChange,
  provider,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  provider: Provider | null; // null = create mode
}) {
  const isEdit = provider !== null;
  const createProvider = useCreateProvider();
  const updateProvider = useUpdateProvider();

  const {
    register,
    handleSubmit,
    watch,
    reset,
    setValue,
    formState: { errors, isSubmitting },
  } = useForm<FormValues>({
    resolver: zodResolver(formSchema),
    defaultValues: { name: "", base_url: "", auth_type: "api_key", api_key: "" },
  });

  const authType = watch("auth_type");

  useEffect(() => {
    if (open) {
      reset(
        provider
          ? { name: provider.name, base_url: provider.base_url, auth_type: provider.auth_type, api_key: "" }
          : { name: "", base_url: "", auth_type: "api_key", api_key: "" },
      );
    }
  }, [open, provider, reset]);

  const onSubmit = async (values: FormValues) => {
    if (!isEdit && values.auth_type === "api_key" && !values.api_key) {
      toast.error("该鉴权方式需要填写 API Key");
      return;
    }

    try {
      if (isEdit) {
        await updateProvider.mutateAsync({
          id: provider.id,
          input: {
            name: values.name,
            base_url: values.base_url,
            auth_type: values.auth_type,
            api_key: values.api_key ? values.api_key : undefined,
            is_active: provider.is_active,
          },
        });
        toast.success("供应商已更新");
      } else {
        await createProvider.mutateAsync({
          name: values.name,
          base_url: values.base_url,
          auth_type: values.auth_type,
          api_key: values.api_key,
        });
        toast.success("供应商已创建");
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
          <DialogTitle>{isEdit ? "编辑供应商" : "添加供应商"}</DialogTitle>
          <DialogDescription>接入一个 OpenAI 兼容协议的模型供应商</DialogDescription>
        </DialogHeader>

        <form className="grid gap-4" onSubmit={handleSubmit(onSubmit)}>
          <div className="grid gap-2">
            <Label htmlFor="name">名称</Label>
            <Input id="name" placeholder="如：公司 DeepSeek 账号" {...register("name")} />
            {errors.name && <p className="text-sm text-destructive">{errors.name.message}</p>}
          </div>

          <div className="grid gap-2">
            <Label htmlFor="base_url">Base URL</Label>
            <Input id="base_url" placeholder="https://api.deepseek.com/v1" {...register("base_url")} />
            {errors.base_url && <p className="text-sm text-destructive">{errors.base_url.message}</p>}
          </div>

          <div className="grid gap-2">
            <Label htmlFor="auth_type">鉴权方式</Label>
            <Select
              value={authType}
              onValueChange={(v) => setValue("auth_type", v as "api_key" | "none")}
            >
              <SelectTrigger id="auth_type">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="api_key">API Key</SelectItem>
                <SelectItem value="none">无需鉴权（本地 Ollama/vLLM）</SelectItem>
              </SelectContent>
            </Select>
          </div>

          {authType === "api_key" && (
            <div className="grid gap-2">
              <Label htmlFor="api_key">API Key{isEdit && "（留空则不修改）"}</Label>
              <Input
                id="api_key"
                type="password"
                placeholder={isEdit && provider?.has_api_key ? "••••••••" : "sk-..."}
                {...register("api_key")}
              />
            </div>
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
