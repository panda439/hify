package conversation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// 005-tool-loop-guard：工具调用循环的第二层止损——重复调用检测。
//
// 第一层是 maxToolCallIterations 那个硬计数器，它封的是**成本上限**，
// 区分不了「这个任务合理需要 6 步」和「模型卡在同一个调用上」。真正的死循环
// 特征是**同一个工具 + 同一组参数被反复调用**，这个文件负责识别它。
//
// 本文件刻意不依赖数据库、不依赖 provider，全部是纯函数 + 一个进程内状态机，
// 因此可以零依赖单测（见 toolloop_test.go）。

// maxIdenticalToolCalls 是同一调用连续出现多少次算死循环。
//
// 取 3 而不是 2：合法的重试应当被放过——工具瞬时失败（网络抖动、上游 5xx）
// 之后模型用相同参数再试一次是完全合理的行为，把它判成死循环会误杀。
// 连续第 3 次拿相同参数调同一个工具，几乎不可能是有意的。
const maxIdenticalToolCalls = 3

// normalizeToolArgs 把工具参数规范化成一个可比较的字符串。
//
// 只做**结构级**规范化：能解析成 JSON 就按 key 递归排序后紧凑序列化，
// 于是 {"a":1,"b":2} 与 {"b":2,"a":1} 得到同一个结果。
//
// **刻意不做语义级等价**（{"city":"北京"} vs {"city":"Beijing"}）。理由有两条：
// 一是模型转圈时的重复调用**往往逐字相同**，结构级已经覆盖绝大多数真实死循环；
// 二是「什么算语义等价」没有边界，做进来既会引入无穷的判断，也会误杀合法重试
// （两次参数确实不同、只是看起来像）。宁可漏检，不可误杀——漏检还有第一层
// 计数器兜底，误杀则会打断一个本来能完成的任务。
//
// 参数不是合法 JSON 时（模型偶尔会吐出截断或带多余字符的串）退化为对原始
// 字符串取指纹，不报错：这里的职责是「判断两次调用是否相同」，而不是校验参数
// 合法性——后者是工具执行层的事，让它照常失败并把错误反馈给模型。
func normalizeToolArgs(args string) string {
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return strings.TrimSpace(args)
	}
	var sb strings.Builder
	writeCanonical(&sb, v)
	return sb.String()
}

// writeCanonical 递归写出 JSON 值的规范形式：对象按 key 排序，其余原样。
func writeCanonical(sb *strings.Builder, v any) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sb.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				sb.WriteByte(',')
			}
			// key 本身用 JSON 编码，避免含引号/反斜杠的 key 破坏结构
			kb, _ := json.Marshal(k)
			sb.Write(kb)
			sb.WriteByte(':')
			writeCanonical(sb, t[k])
		}
		sb.WriteByte('}')
	case []any:
		// 数组顺序**有语义**，不排序——[1,2] 和 [2,1] 是不同的调用。
		sb.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				sb.WriteByte(',')
			}
			writeCanonical(sb, e)
		}
		sb.WriteByte(']')
	default:
		b, err := json.Marshal(t)
		if err != nil {
			sb.WriteString(fmt.Sprintf("%v", t))
			return
		}
		sb.Write(b)
	}
}

// toolCallFingerprint 是「工具名 + 规范化参数」的哈希，用于判断两次调用是否相同。
//
// 取哈希而不是直接留原文，是为了让它能安全地进日志与 trace：参数原文可能含
// 用户输入（地址、订单号），而指纹只用于「两次是否相同」的比较，不需要可读。
// 记录时只取前缀（见 toolLoopStats）。
func toolCallFingerprint(name, args string) string {
	sum := sha256.Sum256([]byte(name + "\x00" + normalizeToolArgs(args)))
	return hex.EncodeToString(sum[:])
}

// toolLoopDetector 跟踪单轮对话内的连续重复调用。
//
// **只在单轮内有效**，不跨对话轮持久化：跨轮重复是合理的（用户可能就是又问了
// 一次同样的问题），只有同一轮内的连续重复才是死循环。
//
// 「连续」的定义：出现一个不同的指纹就重新计数。A、A、B、A 不算 A 出现三次，
// 因为中间那次 B 说明模型换过策略、拿到过新信息。
type toolLoopDetector struct {
	lastFingerprint string
	repeatCount     int
	// blocked 是本轮已被拦截、禁止再调用的工具名。
	blocked map[string]bool
}

func newToolLoopDetector() *toolLoopDetector {
	return &toolLoopDetector{blocked: make(map[string]bool)}
}

// isBlocked 报告某个工具是否已在本轮被拦截。
func (d *toolLoopDetector) isBlocked(name string) bool {
	return d.blocked[name]
}

// observe 记录一次即将执行的工具调用，返回是否应当拦截它。
//
// 返回 true 表示这是同一调用的第 maxIdenticalToolCalls 次连续出现，调用方
// MUST NOT 执行它，并且应当把该工具加入黑名单（本方法已经加好）+ 向模型注入
// 一条说明。
func (d *toolLoopDetector) observe(name, args string) bool {
	fp := toolCallFingerprint(name, args)
	if fp == d.lastFingerprint {
		d.repeatCount++
	} else {
		d.lastFingerprint = fp
		d.repeatCount = 1
	}
	if d.repeatCount >= maxIdenticalToolCalls {
		d.blocked[name] = true
		return true
	}
	return false
}

// fingerprintPrefix 返回可安全进日志的指纹前缀（8 个十六进制字符）。
// 足以在一次排查里区分不同调用，又不可逆推参数原文。
func fingerprintPrefix(fp string) string {
	if len(fp) <= 8 {
		return fp
	}
	return fp[:8]
}

// loopInterventionMessage 是拦截后注入给模型的说明。
//
// 措辞要点：告诉它**事实**（同样参数调了几次、没有新信息）+ 给**两条明确出路**
// （换策略 / 直接告诉用户查不到）。只说「不要再调了」是不够的——模型转圈往往
// 正是因为它不知道还能怎么办，得给它一个可执行的下一步。
//
// 注入消息本身也不足够：已知失败模式是模型道歉之后再调一次同样的，所以调用方
// 必须同时把该工具从后续迭代的可用列表里摘掉（见 detector.blocked）。
func loopInterventionMessage(name string, count int) string {
	return fmt.Sprintf(
		"系统提示：工具 %s 已经用完全相同的参数连续调用了 %d 次，每次返回的信息相同，继续调用不会得到新结果。"+
			"该工具在本轮对话中已被停用。请改用其他方式获取所需信息；"+
			"如果没有其他办法，请直接告诉用户你查不到这项信息，不要重复尝试。",
		name, count)
}

// blockedToolResultMessage 是模型在工具被停用后仍然请求调用它时，回给它的工具结果。
//
// 必须回一条内容而不是静默丢弃：工具调用协议要求每个 tool_call 都有配对的
// tool 结果，缺了下一次请求就是畸形的；而且静默丢弃会让模型看不到任何反馈，
// 更容易继续转圈。
func blockedToolResultMessage(name string) string {
	return fmt.Sprintf("工具 %s 已在本轮对话中停用（重复调用过多）。请不要再调用它。", name)
}

// toolLoopExhaustedMessage 是迭代触顶时补的收尾助手消息（第三层止损）。
//
// **由程序拼接，不经过模型。** 两个原因：一是触顶已经是异常路径，再多打一次
// 模型有成本和延迟；二是让模型基于不完整的中间结果作答会诱发填空式幻觉——
// 它会把缺的那部分编出来，而用户拿到的是一个看起来完整的答案。
// 「信息不完整」这句声明必须由程序保证，不能寄望于提示词。
func toolLoopExhaustedMessage() string {
	return "抱歉，我在处理这个问题时连续调用了多次工具仍未能得到完整结果，已停止继续尝试。" +
		"上面的工具调用记录是我已经获取到的信息，可能并不完整。" +
		"你可以换一种问法，或者把问题拆成更小的部分再问我。"
}
