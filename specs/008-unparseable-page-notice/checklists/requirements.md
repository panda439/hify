# Specification Quality Checklist: 解析失败的页面也要能被用户看见

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-09-05
**Feature**: [spec.md](../spec.md)

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

### 这份规格存在的由来值得记一笔

它不是规划出来的，是 **007 的 FR-011 拦出来的**。

007 交付后发现「解析失败的页不产生提示」这个口子，第一反应是把那些页也塞进
`unextracted_pages`——列名字面上完全覆盖得住。FR-011（本期只承载一种情形，
其它警告不得借用这条通道，直到有独立的规格论证过它）挡住了这个动作，
于是被迫去想「它们真的是同一件事吗」，结论是**不是**：

> FR-008 要求提示能让用户判断下一步动作，而两种原因的下一步完全不同——
> 无文本层要做 OCR，解析失败做 OCR 没用（那页是损坏或不受支持的结构）。
> 一条消息服务不了两种下一步。

**一条反抽象约束在一天之内拦下了一次真实的搭便车，并把它逼成了一次真正的设计讨论。**
这是 FR-011 那类约束少见的、能被观察到的收益，值得记下来。

### 有意为之的判断

- **US2（两种原因不被混为一谈）定为 P1**，虽然它看起来像 US1 的展示细节。
  理由是：**它是本功能不并进现有那一列的全部理由**。如果最终两类仍被渲染成一句
  笼统的「有 N 页没进去」，这一整期的设计成本就白付了——还不如当初直接塞进去。
  把它降为 P2，实现阶段一挤压就会退化成那个结果。

- **FR-003（两类列表互不重叠）看起来是废话**，但它是可断言的、且它保证了
  「分别有多少页」这件事不会重复计数。解析失败的页在统计文本层之前就被跳过了，
  所以物理上不可能重叠——**正因为它显然成立，才值得断言：它一旦不成立，
  说明上游的跳过逻辑变了，而那是个没人会注意到的变化。**

- **FR-011 是 007 FR-011 的同一条纪律**，对第三种原因保留同样的门槛。
  本期是第二种，不是第 N 种——两列不构成"每种原因一列"的许可。
  D-001 要求把"第三种原因出现时的判据"写下来，就是为了不让这条纪律靠记忆维持。

### 进入 plan 前

D-001（承载形态与增生边界）、D-002（提示怎么熬过发布交接）、D-003（两条提示怎么排）
三项必须在 `/speckit-plan` 阶段作出并记录理由。其中 D-002 同时修 007 报告 §6.2
记录的已知缺陷，两者是同一个设计问题。
