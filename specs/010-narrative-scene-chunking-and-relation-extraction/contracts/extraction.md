# 010 抽取与身份归一协议

## 1. 第一阶段输出

一个 UTF-8 JSON 对象，严格不允许 Markdown 围栏、重复 key、尾随第二个对象、未知字段：

```json
{
  "mentions": [{"ref":"m1","surface":"李明","occurrence":0},{"ref":"m2","surface":"小李","occurrence":0}],
  "relations": [],
  "alias_proposals": [{"left":"m1","right":"m2","quote":"李明，人称小李","occurrence":0}]
}
```

示例仅为协议夹具，不是阿Q标注。
relations 每项 `{subject_ref,object_ref,type,evidence:[{quote,occurrence}]}`；evidence 1～4 项。
所有数组必须存在，可空；mentions最多32、relations最多64、alias_proposals最多32；
ref为该响应内唯一短字符串≤32 rune，surface非空≤128 rune，quote非空≤2000 rune；occurrence为0起非负整数。
出现位置由程序在当前 chunk 原文精确查找并计数（按rune，允许重叠匹配）；occurrence越界整次拒绝。
没有 tool call、finish_reason=length、非法/过大输出均为失败，不能部分采用。

关系类型恰为标注指南8类；subject/object必须引用存在且原文可定位的mention，不允许代词当已知人物。
原文存在校验→段位置映射→结构类型校验全部通过后才保存有效候选；不因此声称语义正确。
引用不得跨越无来源的 generated_separator；多段支持使用 evidence数组，不拼造一段“原文”。
空 relations 但非空 alias提案合法；全空对象表示本块未抽到关系，也要走合法空结果发布。

## 2. 归一阶段

服务端给当前 mentions、已校验提案、必要的原文、按 plan 限额选出的候选人物及身份引用，
先测渲染长度；超过12000 rune按候选排名删尾部并标 candidate_truncated，绝不截当前 chunk。
若当前正文+固定指令本身超限，该 item失败。

输出 `{decisions:[{mention_ref,action,character_id,new_group,supports,reason_code}]}`，每个mention恰一决策。
action为 new/link/ambiguous；new/ambiguous的character_id为空，link只能指向本次提供的候选ID。
new_group为当前块的新人物组号：只有明确别名提案及原文支持的mentions可共用一组；
ambiguous必须各自独立组号，link的new_group为空。服务端先为合法新组创建一个人物ID，再解析关系端点。
supports 为1～4条 `{source_ref,quote,occurrence}`，source_ref只能是当前块或输入候选已有依据。
new无既有身份时可只给当前mention引用；link必须同时支持当前和候选身份，或有明确别名连接句。
reason_code封闭为 explicit_alias/context_identity/insufficient/contradictory；不能仅回传数字confidence。

纯函数拒绝：不在输入里的 ID、无出处、仅surface相同却无身份依据、被标为歧义的称呼、
“阿贵可能是名字”等假设/否定提案的直接合并。明确否定/假设语句用固定反例验证，
不承诺这些规则能判断任意文学语义；漏抽/误合并归真实质量评价。

无候选且无别名提案的块不需要第二次调用，给各mention建立独立身份，当前块重复同一精确位置只建一次。
对模型link提案做以上完整校验；任一结构不合法整次归一拒绝，候选关系不发布。
归一响应合法但action=ambiguous，保留独立人物并标歧义，属于成功处理，不强合。

## 3. 稳定性和回放

JSON提案输入hash、schema/prompt/身份规则版本与模型配置固定在job；恢复不升级规则。
人物新ID使用UUIDv7，响应回放通过item事务及确定性决策键避免重复生成。
稳定排序用原文位置、ref、现有人物ID；同一已保存响应的校验/证据映射和指标必须可重复。
生成结果跨次不同属于已知属性；提供原始响应和身份决策日志，不以“随机”掩盖数据库不幂等。
