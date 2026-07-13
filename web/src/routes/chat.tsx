import { MessageSquare, Plus } from "lucide-react";
import { Button } from "@/components/ui/button";
import { EmptyState } from "@/components/empty-state";

export function ChatPage() {
  return (
    <div className="-m-6 flex h-[calc(100svh-0px)]">
      <div className="flex w-72 shrink-0 flex-col border-r bg-muted/30 p-3">
        <Button className="mb-3 justify-start" variant="outline">
          <Plus />
          新建对话
        </Button>
        <div className="flex flex-1 items-center justify-center text-center text-sm text-muted-foreground">
          还没有对话记录
        </div>
      </div>
      <div className="flex flex-1 items-center justify-center p-6">
        <EmptyState
          icon={<MessageSquare className="size-5" />}
          title="选择一个 Agent 开始对话"
          description="从左侧新建对话，或者先到「Agent 管理」创建一个 Agent"
        />
      </div>
    </div>
  );
}
