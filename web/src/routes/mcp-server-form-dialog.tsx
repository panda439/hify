import { useEffect, useState } from "react";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { Plus, X } from "lucide-react";
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
import { useCreateMcpServer, useUpdateMcpServer, type McpServer, type Transport } from "@/lib/mcp";

const formSchema = z.object({
  name: z.string().min(1, "请输入服务器名称"),
  transport: z.enum(["stdio", "sse"]),
  command: z.string().optional(),
  args: z.string().optional(), // space-separated, split on submit
  url: z.string().optional(),
});

type FormValues = z.infer<typeof formSchema>;

interface KVRow {
  key: string;
  value: string;
}

function rowsToMap(rows: KVRow[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const row of rows) {
    if (row.key.trim()) out[row.key.trim()] = row.value;
  }
  return out;
}

function KeyValueEditor({
  label,
  rows,
  onChange,
  keyPlaceholder,
  valuePlaceholder,
}: {
  label: string;
  rows: KVRow[];
  onChange: (rows: KVRow[]) => void;
  keyPlaceholder: string;
  valuePlaceholder: string;
}) {
  const updateRow = (i: number, patch: Partial<KVRow>) => {
    onChange(rows.map((r, idx) => (idx === i ? { ...r, ...patch } : r)));
  };
  const removeRow = (i: number) => onChange(rows.filter((_, idx) => idx !== i));
  const addRow = () => onChange([...rows, { key: "", value: "" }]);

  return (
    <div className="grid gap-2">
      <Label>{label}</Label>
      {rows.map((row, i) => (
        <div key={i} className="flex items-center gap-2">
          <Input
            placeholder={keyPlaceholder}
            value={row.key}
            onChange={(e) => updateRow(i, { key: e.target.value })}
          />
          <Input
            placeholder={valuePlaceholder}
            value={row.value}
            onChange={(e) => updateRow(i, { value: e.target.value })}
          />
          <Button type="button" variant="ghost" size="icon" onClick={() => removeRow(i)}>
            <X className="size-4" />
          </Button>
        </div>
      ))}
      <Button type="button" variant="outline" size="sm" className="w-fit" onClick={addRow}>
        <Plus />
        添加一行
      </Button>
    </div>
  );
}

export function McpServerFormDialog({
  open,
  onOpenChange,
  server,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  server: McpServer | null; // null = create mode
}) {
  const isEdit = server !== null;
  const createServer = useCreateMcpServer();
  const updateServer = useUpdateMcpServer();

  const {
    register,
    handleSubmit,
    watch,
    reset,
    setValue,
    formState: { errors, isSubmitting },
  } = useForm<FormValues>({
    resolver: zodResolver(formSchema),
    defaultValues: { name: "", transport: "stdio", command: "", args: "", url: "" },
  });

  const transport = watch("transport");

  // In edit mode, env/headers only get sent (and only overwrite the
  // existing secret values) if the admin explicitly opens this section and
  // fills it in — see UpdateMcpServerInput's doc comment. Left untouched,
  // the section stays collapsed and the field is omitted from the request.
  const [envRows, setEnvRows] = useState<KVRow[]>([]);
  const [envTouched, setEnvTouched] = useState(!isEdit);
  const [headerRows, setHeaderRows] = useState<KVRow[]>([]);
  const [headersTouched, setHeadersTouched] = useState(!isEdit);

  useEffect(() => {
    if (!open) return;
    if (server) {
      reset({ name: server.name, transport: server.transport, command: server.command, args: (server.args ?? []).join(" "), url: server.url });
      setEnvRows([]);
      setEnvTouched(false);
      setHeaderRows([]);
      setHeadersTouched(false);
    } else {
      reset({ name: "", transport: "stdio", command: "", args: "", url: "" });
      setEnvRows([{ key: "", value: "" }]);
      setEnvTouched(true);
      setHeaderRows([{ key: "", value: "" }]);
      setHeadersTouched(true);
    }
  }, [open, server, reset]);

  const onSubmit = async (values: FormValues) => {
    const args = values.args?.trim() ? values.args.trim().split(/\s+/) : undefined;

    if (values.transport === "stdio" && !values.command?.trim()) {
      toast.error("stdio 传输方式必须填写启动命令");
      return;
    }
    if (values.transport === "sse" && !values.url?.trim()) {
      toast.error("sse 传输方式必须填写 URL");
      return;
    }

    try {
      if (isEdit) {
        await updateServer.mutateAsync({
          id: server.id,
          input: {
            name: values.name,
            command: values.command,
            args,
            env: envTouched ? rowsToMap(envRows) : undefined,
            url: values.url,
            headers: headersTouched ? rowsToMap(headerRows) : undefined,
            is_active: server.is_active,
          },
        });
        toast.success("MCP 服务器已更新");
      } else {
        await createServer.mutateAsync({
          name: values.name,
          transport: values.transport,
          command: values.command,
          args,
          env: rowsToMap(envRows),
          url: values.url,
          headers: rowsToMap(headerRows),
        });
        toast.success("MCP 服务器已创建");
      }
      onOpenChange(false);
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "操作失败，请稍后重试");
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>{isEdit ? "编辑 MCP 服务器" : "添加 MCP 服务器"}</DialogTitle>
          <DialogDescription>接入一个 stdio 或 SSE 传输的 MCP 工具服务器</DialogDescription>
        </DialogHeader>

        <form className="grid gap-4" onSubmit={handleSubmit(onSubmit)}>
          <div className="grid gap-2">
            <Label htmlFor="name">名称</Label>
            <Input id="name" placeholder="如：内部天气查询工具" {...register("name")} />
            {errors.name && <p className="text-sm text-destructive">{errors.name.message}</p>}
          </div>

          <div className="grid gap-2">
            <Label htmlFor="transport">传输方式{isEdit && "（创建后不可修改）"}</Label>
            <Select
              disabled={isEdit}
              value={transport}
              onValueChange={(v) => setValue("transport", v as Transport)}
            >
              <SelectTrigger id="transport">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="stdio">stdio（本地/服务器进程命令）</SelectItem>
                <SelectItem value="sse">SSE（远程 HTTP 端点）</SelectItem>
              </SelectContent>
            </Select>
          </div>

          {transport === "stdio" ? (
            <>
              <div className="grid gap-2">
                <Label htmlFor="command">启动命令</Label>
                <Input id="command" placeholder="如：python3 /path/to/server.py" {...register("command")} />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="args">参数（空格分隔，可选）</Label>
                <Input id="args" placeholder="如：--verbose --port 8080" {...register("args")} />
              </div>
              {isEdit && !envTouched ? (
                <div className="grid gap-2">
                  <Label>环境变量</Label>
                  <p className="text-sm text-muted-foreground">
                    已配置 {server?.env_keys?.length ?? 0} 个环境变量（值不显示）。
                    <Button type="button" variant="link" className="h-auto p-0 pl-1" onClick={() => { setEnvRows([{ key: "", value: "" }]); setEnvTouched(true); }}>
                      重新设置
                    </Button>
                  </p>
                </div>
              ) : (
                <KeyValueEditor
                  label="环境变量"
                  rows={envRows}
                  onChange={setEnvRows}
                  keyPlaceholder="变量名，如 API_KEY"
                  valuePlaceholder="变量值"
                />
              )}
            </>
          ) : (
            <>
              <div className="grid gap-2">
                <Label htmlFor="url">URL</Label>
                <Input id="url" placeholder="https://example.com/sse" {...register("url")} />
              </div>
              {isEdit && !headersTouched ? (
                <div className="grid gap-2">
                  <Label>请求头</Label>
                  <p className="text-sm text-muted-foreground">
                    已配置 {server?.header_keys?.length ?? 0} 个请求头（值不显示）。
                    <Button type="button" variant="link" className="h-auto p-0 pl-1" onClick={() => { setHeaderRows([{ key: "", value: "" }]); setHeadersTouched(true); }}>
                      重新设置
                    </Button>
                  </p>
                </div>
              ) : (
                <KeyValueEditor
                  label="请求头"
                  rows={headerRows}
                  onChange={setHeaderRows}
                  keyPlaceholder="Header 名，如 Authorization"
                  valuePlaceholder="Header 值"
                />
              )}
            </>
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
