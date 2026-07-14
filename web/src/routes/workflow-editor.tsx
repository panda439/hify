import { useCallback, useEffect, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import {
  ReactFlow,
  Background,
  Controls,
  MiniMap,
  addEdge,
  applyNodeChanges,
  applyEdgeChanges,
  type Connection,
  type Edge,
  type NodeChange,
  type EdgeChange,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { ArrowLeft, Bot, Flag, GitBranch, Library, Play, Wrench } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { ApiError } from "@/lib/api";
import { useChatModels } from "@/lib/agents";
import { useKnowledgeBases } from "@/lib/knowledge";
import { useActiveMcpTools } from "@/lib/mcp";
import { useAuthStore } from "@/stores/auth";
import {
  useCreateWorkflow,
  useUpdateWorkflow,
  useWorkflow,
  type Definition,
  type Step,
  type StepType,
  type WorkflowStepEvent,
} from "@/lib/workflows";
import { WorkflowStepNode, stepToNodeData, type WorkflowFlowNode, type RunHighlight } from "@/routes/workflow-node";
import { WorkflowNodePanel } from "@/routes/workflow-node-panel";
import { WorkflowRunDialog } from "@/routes/workflow-run-dialog";

const nodeTypes = { workflowStep: WorkflowStepNode };

const ADDABLE_TYPES: { type: StepType; label: string; icon: typeof Bot }[] = [
  { type: "llm_call", label: "模型调用", icon: Bot },
  { type: "knowledge_retrieval", label: "知识库检索", icon: Library },
  { type: "conditional", label: "条件分支", icon: GitBranch },
  { type: "tool_call", label: "工具调用", icon: Wrench },
  { type: "end", label: "结束", icon: Flag },
];

function newNodeID(type: StepType): string {
  return `${type}-${crypto.randomUUID().slice(0, 8)}`;
}

// A brand new, unsaved workflow always starts with exactly one start
// node — dag.go's Validate requires exactly one, and it's not a type a
// user should ever need to add manually since there's only ever one.
function initialNodes(): WorkflowFlowNode[] {
  return [
    {
      id: "start",
      type: "workflowStep",
      position: { x: 250, y: 40 },
      data: { stepType: "start", config: {} },
      deletable: false,
    },
  ];
}

function stepsToFlow(steps: Step[]): { nodes: WorkflowFlowNode[]; edges: Edge[] } {
  const nodes: WorkflowFlowNode[] = [];
  const edges: Edge[] = [];
  steps.forEach((step, i) => {
    const config = { ...(step.config ?? {}) };
    const pos = config._position as { x: number; y: number } | undefined;
    delete config._position;
    nodes.push({
      id: step.id,
      type: "workflowStep",
      position: pos ?? { x: 250, y: 40 + i * 140 },
      data: { ...stepToNodeData(step), config },
      deletable: step.type !== "start",
    });
    if (step.type === "conditional") {
      if (step.next_if_true) {
        edges.push({ id: `${step.id}-true`, source: step.id, sourceHandle: "true", target: step.next_if_true });
      }
      if (step.next_if_false) {
        edges.push({ id: `${step.id}-false`, source: step.id, sourceHandle: "false", target: step.next_if_false });
      }
    } else if (step.next) {
      edges.push({ id: `${step.id}-next`, source: step.id, target: step.next });
    }
  });
  return { nodes, edges };
}

function flowToDefinition(nodes: WorkflowFlowNode[], edges: Edge[]): Definition {
  return {
    steps: nodes.map((n) => {
      const outgoing = edges.filter((e) => e.source === n.id);
      const step: Step = {
        id: n.id,
        type: n.data.stepType,
        config: { ...n.data.config, _position: n.position },
      };
      if (n.data.stepType === "conditional") {
        const t = outgoing.find((e) => e.sourceHandle === "true");
        const f = outgoing.find((e) => e.sourceHandle === "false");
        if (t) step.next_if_true = t.target;
        if (f) step.next_if_false = f.target;
      } else {
        const next = outgoing[0];
        if (next) step.next = next.target;
      }
      return step;
    }),
  };
}

export function WorkflowEditorPage() {
  const { id } = useParams<{ id: string }>();
  const isNew = id === undefined || id === "new";
  const navigate = useNavigate();
  const user = useAuthStore((s) => s.user);

  const { data: workflow } = useWorkflow(isNew ? null : id!);
  const createWorkflow = useCreateWorkflow();
  const updateWorkflow = useUpdateWorkflow();
  const { data: modelsData } = useChatModels();
  const { data: kbData } = useKnowledgeBases();
  const { data: toolsData } = useActiveMcpTools();

  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [nodes, setNodes] = useState<WorkflowFlowNode[]>(initialNodes());
  const [edges, setEdges] = useState<Edge[]>([]);
  const [selectedNodeId, setSelectedNodeId] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [runDialogOpen, setRunDialogOpen] = useState(false);
  const [loadedOnce, setLoadedOnce] = useState(false);

  const canEdit = isNew || (user !== null && workflow !== undefined && (workflow.created_by === user.id || user.role === "admin"));

  useEffect(() => {
    if (workflow && !loadedOnce) {
      setName(workflow.name);
      setDescription(workflow.description);
      const { nodes: n, edges: e } = stepsToFlow(workflow.definition.steps);
      setNodes(n);
      setEdges(e);
      setLoadedOnce(true);
    }
  }, [workflow, loadedOnce]);

  const onNodesChange = useCallback((changes: NodeChange<WorkflowFlowNode>[]) => {
    setNodes((nds) => applyNodeChanges(changes, nds));
  }, []);
  const onEdgesChange = useCallback((changes: EdgeChange[]) => {
    setEdges((eds) => applyEdgeChanges(changes, eds));
  }, []);
  const onConnect = useCallback((params: Connection) => {
    setEdges((eds) => {
      // A step's Next/NextIfTrue/NextIfFalse are singular — connecting a
      // new edge from a handle that already has one replaces it, rather
      // than fanning out (there's no node type in this engine that
      // executes more than one outgoing edge).
      const filtered = eds.filter((e) => !(e.source === params.source && e.sourceHandle === params.sourceHandle));
      return addEdge(params, filtered);
    });
  }, []);

  const selectedNode = nodes.find((n) => n.id === selectedNodeId) ?? null;

  const addNode = (type: StepType) => {
    const id = newNodeID(type);
    const offset = nodes.length * 40;
    setNodes((nds) => [
      ...nds,
      {
        id,
        type: "workflowStep",
        position: { x: 480 + (offset % 200), y: 40 + (offset % 400) },
        data: { stepType: type, config: {} },
        deletable: true,
      },
    ]);
  };

  const updateNodeConfig = (patch: Record<string, unknown>) => {
    if (!selectedNodeId) return;
    setNodes((nds) =>
      nds.map((n) => (n.id === selectedNodeId ? { ...n, data: { ...n.data, config: { ...n.data.config, ...patch } } } : n)),
    );
  };

  const deleteSelectedNode = () => {
    if (!selectedNodeId) return;
    setNodes((nds) => nds.filter((n) => n.id !== selectedNodeId));
    setEdges((eds) => eds.filter((e) => e.source !== selectedNodeId && e.target !== selectedNodeId));
    setSelectedNodeId(null);
  };

  const handleSave = async () => {
    if (!name.trim()) {
      toast.error("请输入工作流名称");
      return;
    }
    setSaving(true);
    try {
      const definition = flowToDefinition(nodes, edges);
      if (isNew) {
        const created = await createWorkflow.mutateAsync({ name, description, definition });
        toast.success("工作流已创建");
        navigate(`/workflows/${created.id}`, { replace: true });
      } else {
        await updateWorkflow.mutateAsync({
          id: id!,
          input: { name, description, definition, is_active: workflow?.is_active ?? true },
        });
        toast.success("工作流已保存");
      }
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "保存失败，请稍后重试");
    } finally {
      setSaving(false);
    }
  };

  // Live-highlights the node currently running/just-finished on the
  // canvas while WorkflowRunDialog's own trace panel shows the same
  // events as a scrollable log — two views of the same stream.
  const handleStepEvent = (event: WorkflowStepEvent) => {
    if (event.type !== "step" || !event.step_id || !event.status) return;
    const status = event.status as RunHighlight;
    setNodes((nds) => nds.map((n) => (n.id === event.step_id ? { ...n, data: { ...n.data, runStatus: status } } : n)));
  };

  return (
    <div className="-m-6 flex h-[calc(100svh-0px)] flex-col">
      <div className="flex items-center gap-3 border-b bg-background px-4 py-2.5">
        <Button variant="ghost" size="icon" onClick={() => navigate("/workflows")}>
          <ArrowLeft className="size-4" />
        </Button>
        <Input
          className="max-w-56 font-medium"
          placeholder="工作流名称"
          value={name}
          onChange={(e) => setName(e.target.value)}
          disabled={!canEdit}
        />
        <Input
          className="max-w-80 text-muted-foreground"
          placeholder="描述（可选）"
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          disabled={!canEdit}
        />
        <div className="ml-auto flex gap-2">
          {!isNew && (
            <Button variant="outline" onClick={() => setRunDialogOpen(true)} disabled={workflow?.is_active === false}>
              <Play />
              运行
            </Button>
          )}
          {canEdit && (
            <Button onClick={handleSave} disabled={saving}>
              {saving ? "保存中..." : "保存"}
            </Button>
          )}
        </div>
      </div>

      <div className="flex flex-1 overflow-hidden">
        {canEdit && (
          <div className="flex w-44 shrink-0 flex-col gap-2 border-r bg-muted/20 p-3">
            <p className="text-xs font-medium text-muted-foreground">添加节点</p>
            {ADDABLE_TYPES.map((t) => (
              <Button key={t.type} variant="outline" size="sm" className="justify-start" onClick={() => addNode(t.type)}>
                <t.icon className="size-4" />
                {t.label}
              </Button>
            ))}
          </div>
        )}

        <div className="flex-1">
          <ReactFlow
            nodes={nodes}
            edges={edges}
            onNodesChange={canEdit ? onNodesChange : undefined}
            onEdgesChange={canEdit ? onEdgesChange : undefined}
            onConnect={canEdit ? onConnect : undefined}
            nodeTypes={nodeTypes}
            nodesConnectable={canEdit}
            nodesDraggable={canEdit}
            elementsSelectable
            onNodeClick={(_, n) => setSelectedNodeId(n.id)}
            onPaneClick={() => setSelectedNodeId(null)}
            fitView
          >
            <Background />
            <Controls />
            <MiniMap />
          </ReactFlow>
        </div>

        <div className="w-80 shrink-0 border-l bg-background">
          <WorkflowNodePanel
            node={canEdit ? selectedNode : null}
            onConfigChange={updateNodeConfig}
            onDelete={deleteSelectedNode}
            chatModels={modelsData?.items ?? []}
            knowledgeBases={(kbData?.items ?? []).filter((kb) => kb.is_active)}
            mcpTools={toolsData?.items ?? []}
          />
        </div>
      </div>

      <WorkflowRunDialog
        open={runDialogOpen}
        onOpenChange={setRunDialogOpen}
        workflow={workflow ?? null}
        onStepEvent={handleStepEvent}
      />
    </div>
  );
}
