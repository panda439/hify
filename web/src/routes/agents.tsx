import { Bot, Plus } from "lucide-react";
import { PageHeader } from "@/components/page-header";
import { EmptyState } from "@/components/empty-state";
import { Button } from "@/components/ui/button";

export function AgentsPage() {
  return (
    <div>
      <PageHeader
        title="Agent 管理"
        description="配置系统提示词、模型和参数，创建可对话的 Agent"
        action={
          <Button>
            <Plus />
            创建 Agent
          </Button>
        }
      />
      <EmptyState
        icon={<Bot className="size-5" />}
        title="还没有创建任何 Agent"
        description="创建一个 Agent 并关联模型供应商后，就可以在「对话」里开始聊天"
        action={
          <Button variant="outline">
            <Plus />
            创建 Agent
          </Button>
        }
      />
    </div>
  );
}
