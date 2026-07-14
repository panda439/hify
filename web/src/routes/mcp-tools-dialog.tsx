import { RefreshCw } from "lucide-react";
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
import { useMcpServerTools, useSyncMcpTools, type McpServer } from "@/lib/mcp";

export function McpToolsDialog({
  open,
  onOpenChange,
  server,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  server: McpServer | null;
}) {
  const serverId = server?.id ?? "";
  const { data, isLoading } = useMcpServerTools(open ? serverId : null);
  const sync = useSyncMcpTools(serverId);

  const tools = data?.items ?? [];

  const handleSync = async () => {
    try {
      const result = await sync.mutateAsync();
      toast.success(`同步完成，共 ${result.items.length} 个工具`);
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "同步失败，请检查服务器是否可连接");
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>工具列表 — {server?.name}</DialogTitle>
          <DialogDescription>点击「同步」连接服务器拉取最新工具列表；不再提供的工具会被自动禁用</DialogDescription>
        </DialogHeader>

        <div className="grid max-h-72 gap-2 overflow-y-auto">
          {isLoading && <p className="text-sm text-muted-foreground">加载中...</p>}
          {!isLoading && tools.length === 0 && (
            <p className="text-sm text-muted-foreground">还没有同步过工具，点击下方按钮开始同步</p>
          )}
          {tools.map((t) => (
            <div key={t.id} className="rounded-md border p-2">
              <div className="flex items-center gap-2">
                <span className="text-sm font-medium">{t.tool_name}</span>
                {!t.is_active && <Badge variant="outline">已停用</Badge>}
              </div>
              {t.description && <p className="mt-1 text-xs text-muted-foreground">{t.description}</p>}
            </div>
          ))}
        </div>

        <Button onClick={handleSync} disabled={sync.isPending}>
          <RefreshCw className={sync.isPending ? "animate-spin" : ""} />
          {sync.isPending ? "同步中..." : "同步工具"}
        </Button>
      </DialogContent>
    </Dialog>
  );
}
