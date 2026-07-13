import { NavLink, Outlet } from "react-router-dom";
import { Bot, Cpu, MessageSquare, Wrench, Workflow, Library } from "lucide-react";
import { Button } from "@/components/ui/button";
import { useAuthStore } from "@/stores/auth";

const navItems = [
  { to: "/", label: "对话", end: true, icon: MessageSquare },
  { to: "/models", label: "模型管理", icon: Cpu },
  { to: "/agents", label: "Agent 管理", icon: Bot },
  { to: "/knowledge", label: "知识库", icon: Library },
  { to: "/mcp", label: "MCP 工具", icon: Wrench },
  { to: "/workflows", label: "工作流", icon: Workflow },
];

export function AppShell() {
  const user = useAuthStore((s) => s.user);
  const logout = useAuthStore((s) => s.logout);

  return (
    <div className="flex min-h-svh">
      <aside className="flex w-60 shrink-0 flex-col border-r bg-sidebar p-4">
        <div className="mb-6 flex items-center gap-2 px-2 text-lg font-semibold text-sidebar-foreground">
          <div className="flex size-6 items-center justify-center rounded-md bg-primary text-xs text-primary-foreground">
            H
          </div>
          Hify
        </div>
        <nav className="flex flex-1 flex-col gap-1">
          {navItems.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              className={({ isActive }) =>
                `flex items-center gap-2 rounded-md px-3 py-2 text-sm ${
                  isActive
                    ? "bg-sidebar-accent font-medium text-sidebar-accent-foreground"
                    : "text-sidebar-foreground/70 hover:bg-sidebar-accent/50 hover:text-sidebar-foreground"
                }`
              }
            >
              <item.icon className="size-4" />
              {item.label}
            </NavLink>
          ))}
        </nav>
        <div className="mt-auto flex flex-col gap-2 border-t pt-4">
          <div className="truncate text-sm text-muted-foreground">{user?.display_name ?? user?.email}</div>
          <Button variant="outline" size="sm" onClick={logout}>
            退出登录
          </Button>
        </div>
      </aside>
      <main className="flex-1 overflow-auto bg-muted/20 p-6">
        <Outlet />
      </main>
    </div>
  );
}
