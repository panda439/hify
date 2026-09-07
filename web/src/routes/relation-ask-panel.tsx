import { useEffect } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  relationDocumentHint,
  useRelationDocuments,
  type RelationDocument,
} from "@/lib/conversations";
import type { RelationCandidateOption } from "@/lib/sse";

// relation-ask-panel.tsx 是聊天页里问「A 和 B 是什么关系」的那一小块
// （010 T035）。
//
// ⭐ 它是一个**显式开关**，不从提问里猜意图。⚠️ 猜错的两个方向后果都不小：
// 该走关系分支却走了普通检索，用户得到一个基于零散片段的含糊回答；
// 不该走却走了，一个普通问题被答成"没有找到这两个人的关系记录"。
// 而无论哪一种，用户都看不出系统做过一次判断。
//
// ⛔ 界面上不出现 job_id / epoch / hash 这类内部术语。

export interface RelationAskState {
  on: boolean;
  documentId: string;
  subject: string;
  object: string;
  subjectId: string;
  objectId: string;
}

export const emptyRelationAsk: RelationAskState = {
  on: false,
  documentId: "",
  subject: "",
  object: "",
  subjectId: "",
  objectId: "",
};

// relationAskReady 判断这一轮能不能真的带上关系选项。
//
// ⚠️ 三个都填了才算。缺一个就发出去，后端会拒（binding:"required"），
// 而用户看到的是一句语焉不详的"请求失败"——按钮该在这时候就是灰的。
export function relationAskReady(s: RelationAskState): boolean {
  return s.on && s.documentId !== "" && s.subject.trim() !== "" && s.object.trim() !== "";
}

export function RelationAskPanel({
  conversationId,
  state,
  onChange,
  candidates,
}: {
  conversationId: string | null;
  state: RelationAskState;
  onChange: (next: RelationAskState) => void;
  candidates: RelationCandidateOption[];
}) {
  // ⚠️ 只有开关打开时才去要书目：这个 Agent 多半一本关系书都没有，
  // 而每次进聊天页都发一个必然为空的请求没有意义。
  const { data } = useRelationDocuments(conversationId, state.on);
  const docs: RelationDocument[] = data?.items ?? [];
  const selected = docs.find((d) => d.document_id === state.documentId);

  useEffect(() => {
    if (data && state.documentId !== "" && !selected) {
      onChange({ ...state, documentId: "" });
    }
  }, [data, onChange, selected, state]);

  return (
    <div className="mx-auto mb-2 max-w-3xl">
      <label className="flex items-center gap-2 text-xs">
        <input
          type="checkbox"
          checked={state.on}
          onChange={(e) => onChange({ ...emptyRelationAsk, on: e.target.checked })}
        />
        <span>问人物关系</span>
      </label>

      {state.on && (
        <div className="mt-2 grid gap-2 rounded-md border p-2">
          {/* ⭐ 书目为空时说清楚**为什么**，而不是给一个空下拉框。
              ⚠️ 空下拉框会让用户以为是加载失败，反复刷新。 */}
          {docs.length === 0 ? (
            <p className="text-xs text-muted-foreground">
              这个助手的知识库里还没有开启人物关系抽取的文档。
              在「知识库 → 管理文档」里上传小说并勾选「同时抽取人物关系」后，
              这里才会出现可选的书。
            </p>
          ) : (
            <>
              <select
                className="w-full rounded-md border bg-background px-2 py-1 text-xs"
                value={state.documentId}
                onChange={(e) => onChange({ ...state, documentId: e.target.value })}
              >
                <option value="">选择一本书…</option>
                {docs.map((d) => (
                  <option key={d.document_id} value={d.document_id}>
                    {d.file_name}
                  </option>
                ))}
              </select>
              {/* ⚠️ 「还没跑完」「已停止」必须在**问之前**就说，
                  而不是等用户问完一次才从答案里得知。 */}
              {selected && relationDocumentHint(selected) !== "" && (
                <p className="text-xs text-amber-600 dark:text-amber-500">
                  {relationDocumentHint(selected)}
                </p>
              )}
              <div className="flex items-center gap-2">
                <Input
                  className="h-8 text-xs"
                  placeholder="第一个人物"
                  value={state.subject}
                  onChange={(e) => onChange({ ...state, subject: e.target.value, subjectId: "" })}
                />
                <span className="shrink-0 text-xs text-muted-foreground">与</span>
                <Input
                  className="h-8 text-xs"
                  placeholder="第二个人物"
                  value={state.object}
                  onChange={(e) => onChange({ ...state, object: e.target.value, objectId: "" })}
                />
              </div>
              {!relationAskReady(state) && (
                <p className="text-xs text-muted-foreground">
                  选择书目并填写两个人物后才能发送关系提问。
                </p>
              )}
              {candidates.length > 0 && (
                <div className="grid gap-1 rounded-md bg-muted/50 p-2 text-xs">
                  <span>请选择具体人物：</span>
                  {candidates.map((candidate) => (
                    <Button
                      key={`${candidate.role}:${candidate.character_id}`}
                      type="button"
                      variant="outline"
                      size="sm"
                      className="justify-start"
                      onClick={() =>
                        onChange({
                          ...state,
                          [candidate.role]: candidate.display_name,
                          [candidate.role === "subject" ? "subjectId" : "objectId"]:
                            candidate.character_id,
                        })
                      }
                    >
                      {candidate.role === "subject" ? "第一个人物" : "第二个人物"}：
                      {candidate.display_name}（原文第 {candidate.first_source_order} 个字符处）
                    </Button>
                  ))}
                </div>
              )}
              {/* ⭐ 同名多人时的澄清**不在这里做**：候选由后端在回答里列出
                  （带首次出现位置），用户看到之后把名字写得更具体再问一次。
                  ⚠️ 在这里预先展开候选需要先查一次，而那次查询的结果与
                  真正提问时可能已经不同——两次之间抽取还在跑。 */}
            </>
          )}
        </div>
      )}
    </div>
  );
}
