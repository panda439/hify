import { useState } from "react";
import { Users, X } from "lucide-react";

import { Button } from "@/components/ui/button";
import { useNarrativeDocuments } from "@/lib/knowledge";
import type { RelationQuery } from "@/lib/sse";

// relation-query-bar.tsx 是聊天页发起人物关系查询的入口（010 T035）。
//
// ⭐ 它是一个**表单**而不是让用户自由提问，因为服务端要的是两个明确的
// 人名和一本书：靠模型从"赵太爷和阿Q什么关系"里猜出这三个值，猜错的时候
// 用户看到的是一个查不出结果的回答，而不是一个可以改的输入框。
//
// ⚠️ 界面上不出现 job id、epoch、character_id 这类内部标识（契约 §4）。
// 歧义候选的 ID 只在 props 里流转，用户看到的是名字和出处。

export interface RelationQueryDraft {
  documentId: string;
  subject: string;
  object: string;
}

// PendingClarification 是上一轮返回的歧义候选。
export interface PendingClarification {
  subject: string;
  object: string;
  documentId: string;
  subjectCandidates: { character_id: string; display_name: string; context: string }[];
  objectCandidates: { character_id: string; display_name: string; context: string }[];
}

export function RelationQueryBar({
  knowledgeBaseIds,
  disabled,
  onSubmit,
}: {
  knowledgeBaseIds: string[];
  disabled: boolean;
  onSubmit: (query: RelationQuery, question: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const { documents, isLoading } = useNarrativeDocuments(open ? knowledgeBaseIds : []);
  const [documentId, setDocumentId] = useState("");
  const [subject, setSubject] = useState("");
  const [object, setObject] = useState("");

  if (!open) {
    return (
      <Button variant="ghost" size="sm" disabled={disabled} onClick={() => setOpen(true)}>
        <Users />
        查人物关系
      </Button>
    );
  }

  const selectedDoc = documentId || documents[0]?.id || "";
  const canSubmit = selectedDoc !== "" && subject.trim() !== "" && object.trim() !== "";

  const submit = () => {
    if (!canSubmit) return;
    const doc = documents.find((d) => d.id === selectedDoc);
    onSubmit(
      { document_id: selectedDoc, subject: subject.trim(), object: object.trim() },
      // 这句话会作为用户消息存进会话历史，所以要读得像人话，
      // 并且**带上书名**——同一段对话里可能问过好几本书。
      `在《${doc?.file_name ?? "所选文档"}》里，${subject.trim()} 和 ${object.trim()} 是什么关系？`,
    );
  };

  return (
    <div className="mb-2 rounded-md border bg-muted/40 p-2 text-sm">
      <div className="flex items-center justify-between">
        <span className="font-medium">查人物关系</span>
        <Button variant="ghost" size="icon-sm" onClick={() => setOpen(false)}>
          <X />
        </Button>
      </div>
      {isLoading && <p className="mt-1 text-xs text-muted-foreground">正在加载可查询的书目...</p>}
      {!isLoading && documents.length === 0 && (
        // ⚠️ 说清楚为什么空，而不是给一个空下拉框：用户没法从空列表里
        // 猜出"要先按场景切分上传"。
        <p className="mt-1 text-xs text-muted-foreground">
          这个助手的知识库里还没有按场景切分的文档。人物关系只能在按场景切分并抽取过的文档里查询。
        </p>
      )}
      {documents.length > 0 && (
        <div className="mt-2 grid gap-2">
          <select
            className="h-8 rounded border bg-background px-2"
            value={selectedDoc}
            onChange={(e) => setDocumentId(e.target.value)}
          >
            {documents.map((d) => (
              <option key={d.id} value={d.id}>
                {d.file_name}
              </option>
            ))}
          </select>
          <div className="flex items-center gap-2">
            <input
              className="h-8 min-w-0 flex-1 rounded border bg-background px-2"
              placeholder="人物一"
              value={subject}
              onChange={(e) => setSubject(e.target.value)}
              maxLength={128}
            />
            <span className="text-muted-foreground">和</span>
            <input
              className="h-8 min-w-0 flex-1 rounded border bg-background px-2"
              placeholder="人物二"
              value={object}
              onChange={(e) => setObject(e.target.value)}
              maxLength={128}
            />
          </div>
          <Button size="sm" disabled={disabled || !canSubmit} onClick={submit}>
            查询
          </Button>
        </div>
      )}
    </div>
  );
}

// ClarificationPicker 是歧义澄清：同一个称呼对应多个人物时，让用户挑一个。
//
// ⭐ 每个候选都带出处（"首次出现于第 N 个片段"），因为同名的两个人**只能**
// 靠出处分辨。只给名字的话，两个选项在用户看来一模一样。
export function ClarificationPicker({
  clarification,
  disabled,
  onPick,
}: {
  clarification: PendingClarification;
  disabled: boolean;
  onPick: (query: RelationQuery, question: string) => void;
}) {
  const [subjectID, setSubjectID] = useState("");
  const [objectID, setObjectID] = useState("");

  const needSubject = clarification.subjectCandidates.length > 0;
  const needObject = clarification.objectCandidates.length > 0;
  const ready = (!needSubject || subjectID !== "") && (!needObject || objectID !== "");

  const submit = () => {
    if (!ready) return;
    onPick(
      {
        document_id: clarification.documentId,
        subject: clarification.subject,
        object: clarification.object,
        subject_character_id: subjectID || undefined,
        object_character_id: objectID || undefined,
      },
      `${clarification.subject} 和 ${clarification.object} 是什么关系？（已指定具体人物）`,
    );
  };

  const group = (
    label: string,
    cands: PendingClarification["subjectCandidates"],
    value: string,
    setValue: (v: string) => void,
  ) => (
    <div className="grid gap-1">
      <span className="text-xs text-muted-foreground">「{label}」是指：</span>
      {cands.map((c) => (
        <label key={c.character_id} className="flex items-center gap-2">
          <input
            type="radio"
            name={`cand-${label}`}
            checked={value === c.character_id}
            onChange={() => setValue(c.character_id)}
          />
          <span>
            {c.display_name}
            <span className="ml-1 text-xs text-muted-foreground">{c.context}</span>
          </span>
        </label>
      ))}
    </div>
  );

  return (
    <div className="mt-2 rounded-md border bg-muted/40 p-2 text-sm">
      <div className="grid gap-2">
        {needSubject && group(clarification.subject, clarification.subjectCandidates, subjectID, setSubjectID)}
        {needObject && group(clarification.object, clarification.objectCandidates, objectID, setObjectID)}
        <Button size="sm" className="justify-self-start" disabled={disabled || !ready} onClick={submit}>
          用选定的人物再查一次
        </Button>
      </div>
    </div>
  );
}
