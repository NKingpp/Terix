package agent

// 工具入参修复：模型把多个 JSON 对象拼成一个字符串时的容错。
//
// # 事故（2026-11-05，三次全流程复现）
//
// 模型一次发多个并行工具调用时，参数会被拼成一条字节串：
//
//	get_kline {"symbol":"BTC-USDT","market":"crypto"}{"symbol":"ETH-USDT","market":"crypto"}
//	→ Error: invalid tool input: input is not valid JSON:
//	  invalid character '{' after top-level value
//
// 于是它退化成空调用 `get_kline {}` → 「缺少 symbol」→ 反复十几轮，
// 全流程卡在选品阶段永远跑不完。
//
// # 为什么在工具层修而不是等 SDK 修
//
// 这个拼接发生在模型输出侧（`use.Input` 就是那串字节），harness 拿到的
// 已经是坏数据。SDK 只能做 schema 校验（必然失败），没法猜「原意是三次调用」。
// 而工具知道自己的语义 —— 取第一个有效 JSON 是安全的：调用方要的
// 「取某标的行情」已经满足了，多出来的那几个本来也该是独立调用。
//
// # 为什么不用 RawInput=true
//
// RawInput 会让工具跳过 harness 的 schema 校验，但 harness 的校验错误信息
// （"input: missing required property \"id\""）对模型是有用的纠正信号。
// 全量关掉会让「真的漏参数」也变成工具自己的模糊报错。
//
// 所以做法是：保留 harness 校验，只在**即将被拒**时先尝试修复。
// 修复后的字节串重新走一遍校验 —— 能过就正常执行，过不了才报原错。

import (
	"bytes"
	"encoding/json"
	"strings"

	actool "github.com/Autumn-27/norma/tool"
)

// repairToolInput 尝试把「多个 JSON 对象拼接」的入参修成第一个有效对象。
//
// 返回修复后的字节串与是否发生了修复。
//
// # 判据
//
// 原串必须是「一个完整 JSON 对象后面还跟着东西」。做法：
//  1. 用 json.Decoder 读第一个 value，记录它结束的字节偏移
//  2. 若偏移之后还有非空白内容，且第一个 value 是合法 JSON 对象 → 截断
//  3. 否则原样返回（不修，让 harness 报原错）
//
// 不处理「被截断的 JSON」（末尾缺 `}`）—— 那种情况无法可靠猜出原意，
// 猜错比报错更糟。
func repairToolInput(raw []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return raw, false
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var first json.RawMessage
	if err := dec.Decode(&first); err != nil {
		// 第一个对象本身就不完整 —— 不修。
		return raw, false
	}
	rest := bytes.TrimSpace(trimmed[dec.InputOffset():])
	if len(rest) == 0 {
		return raw, false // 只有一个对象，本来就没问题
	}
	// 尾巴必须像「另一个 JSON 的开始」，否则可能是别的噪声（注释、围栏），
	// 那种情况交给 harness 报错更清楚。
	if rest[0] != '{' && rest[0] != '[' {
		return raw, false
	}
	// 第一个对象必须能解析成 map（工具入参都是对象）。
	var probe map[string]any
	if err := json.Unmarshal(first, &probe); err != nil {
		return raw, false
	}
	return first, true
}

// repairToolInputNote 是修复成功时附在结果里的说明。
//
// # 为什么要告诉模型
//
// 不说明的话，模型会以为「我一次发三个调用是可行的」—— 下次还这么干。
// 明确告诉它「只执行了第一个，其余请逐个调用」，它才会改行为。
const repairToolInputNote = "（注意：你一次提交了多个参数对象，本工具只执行了第一个。" +
	"请对剩余的参数逐个单独调用。）"

// hasMultipleJSONObjects 报告入参是否含多个拼接的 JSON 对象（不修改它）。
//
// 用于在工具返回结果时判断要不要附 repairToolInputNote。
func hasMultipleJSONObjects(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var first json.RawMessage
	if err := dec.Decode(&first); err != nil {
		return false
	}
	rest := bytes.TrimSpace(trimmed[dec.InputOffset():])
	return len(rest) > 0 && (rest[0] == '{' || rest[0] == '[')
}

// withRepairNote 在文本结果后面追加修复说明。
func withRepairNote(s string) string {
	if strings.Contains(s, repairToolInputNote) {
		return s
	}
	return s + "\n" + repairToolInputNote
}

// normalizeToolInput 修复并校验入参。
//
// 返回 (可用入参, 是否修复过, 错误结果)。
// 错误结果非 nil 时调用方应直接返回它（格式与 harness 的校验错误一致，
// 让模型看到熟悉的纠正信号）。
func normalizeToolInput(schema map[string]any, in json.RawMessage) (json.RawMessage, bool, *actool.Result) {
	if len(bytes.TrimSpace(in)) == 0 {
		// 空入参：给空对象，让 schema 里的 required 报缺哪个字段
		// （比"input 为空"更有指导性）。
		in = json.RawMessage("{}")
	}
	fixed, repaired := repairToolInput(in)

	// null → [] / {} 归一化。
	//
	// # 为什么必须做（2026-11-08 实测）
	//
	// 模型经常用 `null` 表达「这个可选数组我不填」，而 JSON Schema 的
	// `{"type":"array"}` 不接受 null：
	//
	//	{"traffic_refs": null}  →  invalid tool input: traffic_refs: expected array
	//
	// 但语义上「不填」与「填空数组」对调用方是同一件事 —— 工具的入参
	// 都是 `[]db.TrafficRef` 这类 Go 切片，null 与 [] 反序列化后都是 nil/空，
	// 没有行为差异。所以在这一层把 null 归一成空容器，而不是放宽 schema。
	//
	// # 为什么归一化不改 repaired
	//
	// repaired 的语义是「模型把多个调用拼成一条了，只执行了第一个」，
	// 会附带 repairToolInputNote 告诉模型改行为。null 归一化不是那回事 ——
	// 模型没犯错，只是表达方式不同。若也置 repaired=true，模型会收到
	// 「你一次提交了多个参数对象」这条完全不相干的提示。
	//
	// # 为什么不改 schema 加 "null"
	//
	// 加 `"type": ["array","null"]` 会让**所有**这类字段都要跟着改，且
	// 下游代码仍要处理 nil。在入口归一化只有一处，且不改变任何 schema 语义。
	if norm, changed := nullToEmptyContainer(schema, fixed); changed {
		fixed = norm
	}

	// 修复后重新走一遍 schema 校验 —— 与 harness 的校验等价。
	if err := actool.ValidateInput(schema, fixed); err != nil {
		res := actool.Errorf("invalid tool input: " + err.Error())
		return nil, false, &res
	}
	return fixed, repaired, nil
}

// nullToEmptyContainer 把对象里「schema 要求 array/object、但值为 null」的字段
// 换成空容器。返回新字节串与是否改动过。
//
// 只处理顶层属性：工具的入参都是扁平对象，嵌套 null 归一化没有实际需求，
// 而且递归会带来「把用户真的想要的 null 改掉」的风险 —— 顶层可选字段是
// 模型表达「不填」的唯一位置，改它安全。
func nullToEmptyContainer(schema map[string]any, in json.RawMessage) (json.RawMessage, bool) {
	props, ok := schema["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return in, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(in, &obj); err != nil {
		return in, false // 不是对象，交给后面的校验报错
	}
	changed := false
	for name, spec := range props {
		raw, present := obj[name]
		if !present {
			continue
		}
		if string(bytes.TrimSpace(raw)) != "null" {
			continue
		}
		fieldSchema, ok := spec.(map[string]any)
		if !ok {
			continue
		}
		switch fieldSchema["type"] {
		case "array":
			obj[name] = json.RawMessage("[]")
			changed = true
		case "object":
			obj[name] = json.RawMessage("{}")
			changed = true
		}
	}
	if !changed {
		return in, false
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return in, false
	}
	return out, true
}
