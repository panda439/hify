# Specification Quality Checklist: 叙事文本的场景分块与人物关系抽取

**Created**: 2026-09-06 | **Feature**: [spec.md](../spec.md)

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

### ⚠️ 这一期与 006~009 有三处本质不同

**一、它是第一次把外部模型调用引进入库链路。**
前四期的宪法第 V 条「LLM 降级/超时/开关」全部判 N/A，依据是"没有引入模型调用"。
本期一引入，那半条**立刻从 N/A 变成必须满足**，而且是硬要求：抽取失败绝不能让文档
整体不可用（FR-018）——事实检索那条路必须活着。D-002 要在 plan 里把它定死。

**二、交付物包含两个数字，而不只是功能。**
US4 被定为 P1 不是凑数：这套方案值不值得用，全部依据就是「多少钱」和「多准」。
**只交付功能、给不出这两个数字，本期视为未完成**（写进了 Assumptions）。

**三、⭐ 与 009 相反，本期的质量是可测的。**
009 那次"模型会不会过度对冲"本仓库真测不了，只能承认。而三元组抽取**可以**测——
挑公版书、人工标一小段真值、算准确率召回率。**因此不允许用"测不了"来免除度量责任**：
它测得了，只是要花人工。

### 有意为之的判断

- **US1/US2/US3/US4 四条全是 P1**，看起来违反"优先级要有区分"的直觉。理由：
  US1 是 US2 的输入（块被切断则抽取输入残缺）、US3 是 US2 的前提（不归一则关系碎成孤岛），
  US4 是全期的判断依据。**去掉任何一条，剩下的都不成立或无法判断价值。**
  真要砍，砍的是整个 US2/3/4 这一路（只留 US1 的场景切分），而不是砍其中一条。

- **FR-013「宁可碎，不可错合」与 006 的噪音剥离同一条纪律**：
  两个人被错误合并，产生的是一个**看起来正常但完全错误**的人物；而没合并只是碎，
  用户还能自己看出来。方向不对称，取舍就不对称。

- **SC-007 要求标注"模型/书目/规模"三个前提**。脱离这三者的准确率数字毫无意义，
  而且极容易被当成通用结论到处引用——那比没有数字更糟。

- **Assumptions 里明写"准确率不会很高，这是预期而不是失败"**。本期的目标是**知道它错到
  什么程度**，不是让它不错。这条如果不写在前面，实现阶段一定会滑向"调 prompt 刷分"。

### 进入 plan 前

D-001~D-004 必须在 `/speckit-plan` 阶段作出并记录理由。其中 **D-002（模型调用的降级/超时/开关）
是宪法第 V 条的硬要求**，D-003（真值集口径）不定死则 SC-007 算出来的数字没有意义。
