# Specification Quality Checklist: 让模型知道自己证据的边界

**Created**: 2026-09-05 | **Feature**: [spec.md](../spec.md)

## Content Quality

- [x] No implementation details (languages, frameworks, APIs)
- [x] Focused on user value and business needs
- [x] Written for non-technical stakeholders
- [x] All mandatory sections completed

## Requirement Completeness

- [x] No [NEEDS CLARIFICATION] markers remain
- [x] Requirements are testable and unambiguous
- [x] Success criteria are measurable
- [x] Success criteria are technology-agnostic
- [x] All acceptance scenarios are defined
- [x] Edge cases are identified
- [x] Scope is clearly bounded
- [x] Dependencies and assumptions identified

## Feature Readiness

- [x] All functional requirements have clear acceptance criteria
- [x] User scenarios cover primary flows
- [x] Feature meets measurable outcomes defined in Success Criteria
- [x] No implementation details leak into specification

## Notes

### ⚠️ 这一期与 006/007/008 有一个本质区别，必须被看见

前三期的成功标准全部是**可判定的**：片段有没有被切开、页码区间对不对、字段有没有被带出来。
断言写下去，要么绿要么红。

**本期的主要风险落在唯一没有度量的地方**：模型拿到这些信号后会不会**过度声明不确定**——
对本来答得很好的问题也开始加免责声明。而本仓库：

- `make eval` 带 LLM 裁判，同一份代码跑两次都不一致，宪法明确禁止拿它当"行为未变"的证据；
- 确定性检索门禁只覆盖**检索**，不覆盖**回答**。

所以 SC-001~SC-008 全部是**机制证明**（信号确实到了模型面前、完整文档上是 0 条、预算算对了），
**没有一条能证明"回答变好了"**。这不是规格写漏了，是这个仓库目前的能力边界。

D-001 因此被写成一条必须在 plan 阶段正面回答的开放决策，而不是留给实现阶段自己发挥。
可接受的答案包括"承认只有机制证明，并说明凭什么仍然值得做"——**不可接受的是含糊过去，
或者事后拿 make eval 的数字充当证据**。

### 有意为之的判断

- **US1 与 US2 都是 P1**，虽然 US2 是 007/008 那条线索的终点、看起来更"正统"。
  US1 覆盖的是**每一次**空检索，且不依赖前两期的任何数据；它是"一本正经编答案"最直接的
  土壤，收益面比 US2 宽得多。两者也确实可以独立交付。

- **FR-007（不猜相关性）和 FR-012（不完整不是拒答理由）是两条防滥用要求**，方向相反：
  前者防系统替用户判断、后者防模型拿它当借口。**FR-012 写在提示词里就意味着它是尽力而为**，
  spec 的 Assumptions 已明写这一点，不得在报告里包装成"已解决"。

- **FR-003（检索失败时不得让模型以为"知识库里没有"）容易被漏**。空检索有三种成因，
  只有一种是"确实没匹配"。把降级失败也说成"没有找到"，是在用一个基础设施故障冒充一个事实结论。
