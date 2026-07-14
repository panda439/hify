import { useState } from "react";
import { Plus, Wrench } from "lucide-react";
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
import { useMcpServers, useUpdateMcpServer, type McpServer } from "@/lib/mcp";
import { McpServerFormDialog } from "@/routes/mcp-server-form-dialog";
import { McpToolsDialog } from "@/routes/mcp-tools-dialog";

function statusBadge(server: McpServer) {
  if (!server.is_active) return <Badge variant="outline">已禁用</Badge>;
  switch (server.status) {
    case "connected":
      return (
        <Badge className="border-transparent bg-emerald-100 text-emerald-700 dark:bg-emerald-950 dark:text-emerald-400">
          连接正常
        </Badge>
      );
    case "failed":
      return <Badge variant="destructive">连接失败</Badge>;
    default:
      return <Badge variant="secondary">尚未同步</Badge>;
  }
}

export function McpPage() {
  const { data, isLoading } = useMcpServers();
  const updateServer = useUpdateMcpServer();
  const [dialogOpen, setDialogOpen] = useState(false);
  const [editing, setEditing] = useState<McpServer | null>(null);
  const [toolsDialogOpen, setToolsDialogOpen] = useState(false);
  const [managingTools, setManagingTools] = useState<McpServer | null>(null);
  const [deleting, setDeleting] = useState<McpServer | null>(null);

  const servers = data?.items ?? [];

  const openCreate = () => {
    setEditing(null);
    setDialogOpen(true);
  };

  const openEdit = (s: McpServer) => {
    setEditing(s);
    setDialogOpen(true);
  };

  const openTools = (s: McpServer) => {
    setManagingTools(s);
    setToolsDialogOpen(true);
  };

  // Same soft-disable pattern as provider/agent — no hard DELETE endpoint,
  // "删除" maps to is_active=false so agent_mcp_tools associations don't
  // end up pointing at nothing. Env/headers are omitted here (not
  // resubmitted), which UpdateServer treats as "leave unchanged" — see
  // mcp-server-form-dialog.tsx's doc comment.
  const handleConfirmDelete = async () => {
    if (!deleting) return;
    try {
      await updateServer.mutateAsync({
        id: deleting.id,
        input: {
          name: deleting.name,
          command: deleting.command,
          args: deleting.args ?? undefined,
          url: deleting.url,
          is_active: false,
        },
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
        title="MCP 工具"
        description="接入 MCP 服务器，同步其提供的工具供 Agent 调用"
        action={
          <Button onClick={openCreate}>
            <Plus />
            添加服务器
          </Button>
        }
      />

      {!isLoading && servers.length === 0 && (
        <EmptyState
          icon={<Wrench className="size-5" />}
          title="还没有配置任何 MCP 服务器"
          description="添加一个 stdio 或 SSE 传输的 MCP 服务器后，同步工具即可供 Agent 使用"
          action={
            <Button variant="outline" onClick={openCreate}>
              <Plus />
              添加服务器
            </Button>
          }
        />
      )}

      {servers.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>名称</TableHead>
              <TableHead>传输方式</TableHead>
              <TableHead>状态</TableHead>
              <TableHead className="text-right">操作</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {servers.map((s) => (
              <TableRow key={s.id}>
                <TableCell className="font-medium">{s.name}</TableCell>
                <TableCell className="text-muted-foreground">{s.transport === "stdio" ? "stdio" : "SSE"}</TableCell>
                <TableCell>{statusBadge(s)}</TableCell>
                <TableCell className="text-right">
                  <div className="flex justify-end gap-2">
                    <Button variant="outline" size="sm" onClick={() => openTools(s)}>
                      同步工具
                    </Button>
                    <Button variant="outline" size="sm" onClick={() => openEdit(s)}>
                      编辑
                    </Button>
                    <Button variant="outline" size="sm" onClick={() => setDeleting(s)}>
                      删除
                    </Button>
                  </div>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}

      <McpServerFormDialog open={dialogOpen} onOpenChange={setDialogOpen} server={editing} />
      <McpToolsDialog open={toolsDialogOpen} onOpenChange={setToolsDialogOpen} server={managingTools} />

      <AlertDialog open={deleting !== null} onOpenChange={(open) => !open && setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除服务器「{deleting?.name}」？</AlertDialogTitle>
            <AlertDialogDescription>
              删除后该服务器会被禁用，不再出现在可选列表里；已经引用它的 Agent 不受影响，历史对话不会丢失。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction variant="destructive" onClick={handleConfirmDelete} disabled={updateServer.isPending}>
              删除
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
