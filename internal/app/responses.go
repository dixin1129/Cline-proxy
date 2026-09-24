package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// ============================================================================
// OpenAI Responses API (/v1/responses) -> chat/completions 转换
// 支持 Cursor 等客户端直连反代,无需 opencode CLI
// ============================================================================

// responsesToChat 将 Responses 请求体转换为 chat.completions 请求体
func responsesToChat(body map[string]any) map[string]any {
	out := map[string]any{}
	if m, ok := body["model"].(string); ok {
		out["model"] = m
	}
	if s, ok := body["stream"].(bool); ok {
		out["stream"] = s
	}
	if mt, ok := body["max_output_tokens"].(float64); ok {
		out["max_tokens"] = int(mt)
	}
	for _, k := range []string{"temperature", "top_p", "stop", "seed", "user", "metadata", "logit_bias"} {
		if v, ok := body[k]; ok {
			out[k] = v
		}
	}
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort != "" {
			out["reasoning_effort"] = effort
		}
	}
	// 流式请求显式索取 usage，否则上游通常不下发 token 统计，
	// 客户端(Codex)会一直认为上下文为空而永不触发自动压缩。
	if s, _ := body["stream"].(bool); s {
		out["stream_options"] = mergeStreamOptions(body["stream_options"])
	}
	if model, _ := body["model"].(string); model == "" || strings.HasPrefix(model, "z-ai/glm-") {
		out["enable_thinking"] = true
	}
	if instr, ok := body["instructions"].(string); ok && instr != "" {
		out["messages"] = append([]any{map[string]any{"role": "system", "content": instr}}, responsesInputToMessages(body["input"])...)
	} else {
		out["messages"] = responsesInputToMessages(body["input"])
	}
	if tools, ok := body["tools"].([]any); ok {
		out["tools"] = responsesToolsToChat(tools)
	}
	if tc, ok := body["tool_choice"]; ok {
		out["tool_choice"] = tc
	}
	return out
}

func responsesInputToMessages(input any) []any {
	var msgs []any
	switch v := input.(type) {
	case string:
		msgs = append(msgs, map[string]any{"role": "user", "content": v})
	case []any:
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "message":
				role, _ := m["role"].(string)
				if role == "" {
					role = "user"
				}
				msgs = append(msgs, map[string]any{"role": role, "content": stringifyResponsesContent(m["content"])})
			case "function_call":
				callID, _ := m["call_id"].(string)
				if callID == "" {
					callID, _ = m["id"].(string)
				}
				name, _ := m["name"].(string)
				args := ""
				switch a := m["arguments"].(type) {
				case string:
					args = a
				case map[string]any:
					if b, err := json.Marshal(a); err == nil {
						args = string(b)
					}
				}
				msgs = append(msgs, map[string]any{
					"role":       "assistant",
					"content":    "",
					"tool_calls": []any{map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": args}}},
				})
			case "function_call_output":
				callID, _ := m["call_id"].(string)
				if callID == "" {
					callID, _ = m["id"].(string)
				}
				output := ""
				switch o := m["output"].(type) {
				case string:
					output = o
				case map[string]any:
					if b, err := json.Marshal(o); err == nil {
						output = string(b)
					}
				}
				msgs = append(msgs, map[string]any{"role": "tool", "content": output, "tool_call_id": callID})
			case "reasoning":
				// 忽略 Reasoning 输入项(无法映射到 chat 输入)
			}
		}
	}
	return msgs
}

func stringifyResponsesContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		parts := []string{}
		for _, block := range v {
			if b, ok := block.(map[string]any); ok {
				if t, ok := b["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func responsesToolsToChat(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if tm["type"] == "function" {
			fn := map[string]any{}
			if n, ok := tm["name"].(string); ok {
				fn["name"] = n
			}
			if d, ok := tm["description"].(string); ok {
				fn["description"] = d
			}
			if p, ok := tm["parameters"].(map[string]any); ok {
				fn["parameters"] = p
			}
			out = append(out, map[string]any{"type": "function", "function": fn})
		}
	}
	return out
}

// mergeStreamOptions 在保留客户端已有 stream_options 的前提下强制打开 include_usage。
func mergeStreamOptions(raw any) map[string]any {
	out := map[string]any{}
	if m, ok := raw.(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	out["include_usage"] = true
	return out
}

// ============ 非流式响应转换 ============

// chatToResponses chat.completions 响应 -> Responses 响应。
// fallbackInputTokens 可选：上游未返回 usage 时用于输入 token 估算。
func chatToResponses(chat map[string]any, fallbackInputTokens ...int) map[string]any {
	resp := map[string]any{
		"id":          "resp_" + fmt.Sprintf("%x", time.Now().UnixMilli()),
		"object":      "response",
		"created_at":  time.Now().Unix(),
		"status":      "completed",
		"model":       chat["model"],
		"output":      []any{},
		"output_text": "",
	}
	choices, _ := chat["choices"].([]any)
	outputs := []any{}
	var outputText strings.Builder
	var reasoningText strings.Builder
	toolArgs := []string{}
	if len(choices) > 0 {
		if ch, ok := choices[0].(map[string]any); ok {
			msg, _ := ch["message"].(map[string]any)
			if msg == nil {
				msg, _ = ch["delta"].(map[string]any)
			}
			// 推理内容与正文分开建模，保持与流式路径一致的 output 结构
			reasoning, _ := msg["reasoning_content"].(string)
			if reasoning == "" {
				reasoning, _ = msg["reasoning"].(string)
			}
			if reasoning != "" {
				reasoningText.WriteString(reasoning)
				outputs = append(outputs, reasoningItem("rs_"+fmt.Sprintf("%x", time.Now().UnixNano()), reasoning))
			}
			content := []any{}
			if c, ok := msg["content"].(string); ok && c != "" {
				outputText.WriteString(c)
				content = append(content, map[string]any{"type": "output_text", "text": c, "annotations": []any{}})
			}
			msgOut := map[string]any{
				"type":        "message",
				"id":          "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli()),
				"status":      "completed",
				"role":        "assistant",
				"content":     content,
				"output_text": outputText.String(),
			}
			outputs = append(outputs, msgOut)

			if tc, ok := msg["tool_calls"].([]any); ok {
				for _, c := range tc {
					if cm, ok := c.(map[string]any); ok {
						fn, _ := cm["function"].(map[string]any)
						callID, _ := cm["id"].(string)
						if callID == "" {
							callID = fmt.Sprintf("fc_%x", time.Now().UnixNano())
						}
						name := ""
						args := ""
						if fn != nil {
							name, _ = fn["name"].(string)
							if a, ok := fn["arguments"].(string); ok {
								args = a
							}
						}
						if args != "" {
							toolArgs = append(toolArgs, args)
						}
						outputs = append(outputs, map[string]any{
							"type":      "function_call",
							"id":        "fc_" + fmt.Sprintf("%x", time.Now().UnixMilli()),
							"call_id":   callID,
							"name":      name,
							"arguments": args,
							"status":    "completed",
						})
					}
				}
			}
		}
	}
	resp["output"] = outputs
	resp["output_text"] = outputText.String()
	fallback := 0
	if len(fallbackInputTokens) > 0 {
		fallback = fallbackInputTokens[0]
	}
	resp["usage"] = chatUsageToResponsesUsage(chat["usage"], fallback, outputText.String(), reasoningText.String(), toolArgs)
	return resp
}

// chatUsageToResponsesUsage 把 chat.completions 的 usage 映射为 Responses 结构。
// 缺失时按请求与产出内容估算，保证客户端拿到非 0 的稳定字段。
func chatUsageToResponsesUsage(raw any, fallbackInputTokens int, outText, reasoningText string, toolArgs []string) map[string]any {
	u, _ := raw.(map[string]any)
	prompt := intField(u, "prompt_tokens")
	completion := intField(u, "completion_tokens")
	total := intField(u, "total_tokens")
	if prompt == 0 && completion == 0 && total == 0 {
		prompt = fallbackInputTokens
		completion = estimateTextTokens(outText) + estimateTextTokens(reasoningText)
		for _, args := range toolArgs {
			completion += estimateTextTokens(args)
		}
		total = prompt + completion
		if prompt > 0 || completion > 0 {
			log.Printf("  responses usage: non-stream upstream not provided, estimated input=%d output=%d", prompt, completion)
		}
	}
	if total == 0 {
		total = prompt + completion
	}
	cached := 0
	if pd, ok := u["prompt_tokens_details"].(map[string]any); ok {
		cached = intField(pd, "cached_tokens")
	}
	reasoning := intField(u, "reasoning_tokens")
	if cd, ok := u["completion_tokens_details"].(map[string]any); ok && reasoning == 0 {
		reasoning = intField(cd, "reasoning_tokens")
	}
	return map[string]any{
		"input_tokens": prompt,
		"input_tokens_details": map[string]any{
			"cached_tokens": cached,
		},
		"output_tokens": completion,
		"output_tokens_details": map[string]any{
			"reasoning_tokens": reasoning,
		},
		"total_tokens": total,
	}
}

// ============ 流式响应转换 (Responses SSE) ============

type responsesSSEWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	msgID   string
	respID  string
}

func newResponsesSSE(w http.ResponseWriter) *responsesSSEWriter {
	f, _ := w.(http.Flusher)
	return &responsesSSEWriter{w: w, flusher: f, msgID: "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli()), respID: "resp_" + fmt.Sprintf("%x", time.Now().UnixMilli())}
}

func (s *responsesSSEWriter) event(event string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, string(b))
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// callAccumulator 累积单个工具调用的流式片段。
// 上游用 index 区分并行调用; 缺失 index 时按 call_id / name 归并，
// 绝不把多个调用的 arguments 拼进同一个字符串。
type callAccumulator struct {
	callID   string
	name     string
	args     strings.Builder
	outputID string
	outIndex int
	emitted  bool
}

// callRegistry 按上游 index / call_id 分桶管理工具调用。
type callRegistry struct {
	byIndex map[int]*callAccumulator
	byKey   map[string]*callAccumulator
	order   []*callAccumulator
}

func newCallRegistry() *callRegistry {
	return &callRegistry{
		byIndex: map[int]*callAccumulator{},
		byKey:   map[string]*callAccumulator{},
	}
}

// slot 返回本次 delta 应写入的调用桶，必要时新建。
//
// 分桶规则(按优先级):
//  1. 带 index: 归入该 index; 若同一 index 出现不同 call_id 且旧桶已有内容，
//     说明上游复用了 index 发起了新调用，另起一桶。
//  2. 不带 index 但带 call_id: 按 call_id 归并。
//  3. 两者都没有: name 非空视为新调用起点，否则续写最近一桶(兼容纯增量参数)。
func (r *callRegistry) slot(index *int, callID, name string, hasName bool) *callAccumulator {
	if index != nil {
		if acc, ok := r.byIndex[*index]; ok {
			if callID == "" || acc.callID == "" || acc.callID == callID {
				return acc
			}
			if acc.args.Len() == 0 {
				if acc.callID != "" {
					delete(r.byKey, acc.callID)
				}
				acc.callID = callID
				r.byKey[callID] = acc
				return acc
			}
			return r.start(callID, name, index)
		}
		return r.start(callID, name, index)
	}
	if callID != "" {
		if acc, ok := r.byKey[callID]; ok {
			return acc
		}
		return r.start(callID, name, nil)
	}
	if hasName || len(r.order) == 0 {
		return r.start("", name, nil)
	}
	return r.order[len(r.order)-1]
}

// start 新建调用桶；index 非空时按其归属登记，避免与无 index 的流混用时串桶。
func (r *callRegistry) start(callID, name string, index *int) *callAccumulator {
	acc := &callAccumulator{
		callID:   callID,
		name:     name,
		outputID: "fc_" + fmt.Sprintf("%x", time.Now().UnixNano()),
	}
	r.order = append(r.order, acc)
	if index != nil {
		r.byIndex[*index] = acc
	}
	if callID != "" {
		r.byKey[callID] = acc
	}
	return acc
}

// bindID 记录调用桶的 call_id，使后续不带 index 的片段也能按 id 命中同一桶。
func (r *callRegistry) bindID(acc *callAccumulator, id string) {
	if acc == nil || id == "" || acc.callID == id {
		return
	}
	acc.callID = id
	r.byKey[id] = acc
}

// all 按首次出现顺序返回全部调用桶。
func (r *callRegistry) all() []*callAccumulator {
	out := make([]*callAccumulator, 0, len(r.order))
	for _, acc := range r.order {
		if acc == nil {
			continue
		}
		out = append(out, acc)
	}
	return out
}

// responseUsage 归一化后的 usage，供 response.completed 使用。
type responseUsage struct {
	inputTokens     int
	outputTokens    int
	totalTokens     int
	cachedTokens    int
	reasoningTokens int
	estimated       bool
}

// streamUsageTracker 捕获上游 SSE 中的 usage；上游未提供时按产出文本估算兜底。
type streamUsageTracker struct {
	usage     responseUsage
	haveUsage bool
}

// observe 记录上游下发的 usage（多次出现时以最后一次为准）。
func (t *streamUsageTracker) observe(u map[string]any) {
	if u == nil {
		return
	}
	prompt := intField(u, "prompt_tokens")
	completion := intField(u, "completion_tokens")
	total := intField(u, "total_tokens")
	if prompt == 0 && completion == 0 && total == 0 {
		return
	}
	cached := 0
	if pd, ok := u["prompt_tokens_details"].(map[string]any); ok {
		cached = intField(pd, "cached_tokens")
	}
	reasoning := intField(u, "reasoning_tokens")
	if cd, ok := u["completion_tokens_details"].(map[string]any); ok && reasoning == 0 {
		reasoning = intField(cd, "reasoning_tokens")
	}
	if total == 0 {
		total = prompt + completion
	}
	t.usage = responseUsage{
		inputTokens:     prompt,
		outputTokens:    completion,
		totalTokens:     total,
		cachedTokens:    cached,
		reasoningTokens: reasoning,
	}
	t.haveUsage = true
}

// finish 返回最终 usage；上游未给时用入站请求与已产出内容估算，绝不返回全 0。
// extraTexts 为工具调用参数等未计入 outText 的产出内容。
func (t *streamUsageTracker) finish(fallbackInputTokens int, outText, reasoningText string, extraTexts ...string) responseUsage {
	if t.haveUsage {
		u := t.usage
		if u.totalTokens == 0 {
			u.totalTokens = u.inputTokens + u.outputTokens
		}
		if u.reasoningTokens == 0 && reasoningText != "" {
			u.reasoningTokens = estimateTextTokens(reasoningText)
		}
		return u
	}
	out := estimateTextTokens(outText) + estimateTextTokens(reasoningText)
	for _, extra := range extraTexts {
		out += estimateTextTokens(extra)
	}
	return responseUsage{
		inputTokens:     fallbackInputTokens,
		outputTokens:    out,
		totalTokens:     fallbackInputTokens + out,
		reasoningTokens: estimateTextTokens(reasoningText),
		estimated:       true,
	}
}

// intField 从 JSON 解析出的 map 里读取整数字段（JSON 数字统一是 float64）。
func intField(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

// chatStreamToResponses 将上游 chat.completions SSE 流转换为 Responses SSE 流。
// onUsage 用于账号记账；fallbackInputTokens 是上游未给 usage 时的入站请求估算值。
func chatStreamToResponses(w http.ResponseWriter, upstream *http.Response, onUsage func(map[string]any), fallbackInputTokens int, reqModel string) {
	model := reqModel
	s := newResponsesSSE(w)
	s.event("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":         s.respID,
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "in_progress",
			"model":      model,
			"output":     []any{},
		},
	})
	s.event("response.in_progress", map[string]any{"type": "response.in_progress", "response": map[string]any{"id": s.respID}})

	// output_index 由统一的分配器发放，保证每个 item 的 index 唯一且递增。
	nextIndex := 0
	allocIndex := func() int {
		i := nextIndex
		nextIndex++
		return i
	}

	textEmitted := false
	messageIndex := -1
	textDone := false
	reasoningEmitted := false
	reasoningClosed := false
	reasoningIndex := -1
	var reasoningText strings.Builder
	var outText strings.Builder
	reasoningID := "rs_" + fmt.Sprintf("%x", time.Now().UnixNano())
	messageID := "msg_" + fmt.Sprintf("%x", time.Now().UnixNano())
	calls := newCallRegistry()
	usage := &streamUsageTracker{}

	closeReasoning := func() {
		if !reasoningEmitted || reasoningClosed {
			return
		}
		reasoningClosed = true
		idx := reasoningIndex
		s.event("response.reasoning_summary_text.done", map[string]any{
			"type":          "response.reasoning_summary_text.done",
			"item_id":       reasoningID,
			"output_index":  idx,
			"summary_index": 0,
			"content_index": 0,
			"text":          reasoningText.String(),
		})
		s.event("response.reasoning_summary_part.done", map[string]any{
			"type":          "response.reasoning_summary_part.done",
			"item_id":       reasoningID,
			"output_index":  idx,
			"summary_index": 0,
			"part": map[string]any{
				"type": "summary_text",
				"text": reasoningText.String(),
			},
		})
		s.event("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": idx,
			"item":         reasoningItem(reasoningID, reasoningText.String()),
		})
	}

	closeText := func() {
		if !textEmitted || textDone {
			return
		}
		textDone = true
		s.event("response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": messageID, "output_index": messageIndex, "content_index": 0, "text": outText.String()})
		s.event("response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": messageID, "output_index": messageIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": outText.String(), "annotations": []any{}}})
		s.event("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": messageIndex, "item": messageItem(messageID, outText.String())})
	}

	// emitCall 在首次拿到 name 时发 output_item.added，并按出现顺序分配 index。
	emitCall := func(acc *callAccumulator) {
		if acc == nil || acc.emitted || acc.name == "" {
			return
		}
		if acc.callID == "" {
			acc.callID = "call_" + fmt.Sprintf("%x", time.Now().UnixNano())
		}
		acc.emitted = true
		closeReasoning()
		acc.outIndex = allocIndex()
		s.event("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": acc.outIndex,
			"item": map[string]any{
				"type":      "function_call",
				"id":        acc.outputID,
				"call_id":   acc.callID,
				"name":      acc.name,
				"arguments": "",
				"status":    "in_progress",
			},
		})
	}

	reader := bufio.NewReader(upstream.Body)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "data:") {
				payload := strings.TrimSpace(line[5:])
				if payload == "" || payload == "[DONE]" {
					if err != nil {
						break
					}
					continue
				}
				var obj map[string]any
				if json.Unmarshal([]byte(payload), &obj) != nil {
					if err != nil {
						break
					}
					continue
				}
				if data, ok := obj["data"]; ok {
					if d, ok := data.(map[string]any); ok {
						obj = d
					}
				}
				if m, ok := obj["model"].(string); ok && m != "" {
					model = m
				}
				if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
					usage.observe(u)
					if onUsage != nil {
						onUsage(u)
					}
				}
				choices, _ := obj["choices"].([]any)
				if len(choices) == 0 {
					if err != nil {
						break
					}
					continue
				}
				ch, _ := choices[0].(map[string]any)
				if ch == nil {
					if err != nil {
						break
					}
					continue
				}
				delta, _ := ch["delta"].(map[string]any)
				if delta == nil {
					delta = ch
				}
				// 文本
				if c, ok := delta["content"].(string); ok && c != "" {
					if !textEmitted {
						textEmitted = true
						closeReasoning()
						messageIndex = allocIndex()
						s.event("response.output_item.added", map[string]any{
							"type":         "response.output_item.added",
							"output_index": messageIndex,
							"item":         map[string]any{"id": messageID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}},
						})
						s.event("response.content_part.added", map[string]any{
							"type":          "response.content_part.added",
							"item_id":       messageID,
							"output_index":  messageIndex,
							"content_index": 0,
							"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
						})
					}
					outText.WriteString(c)
					s.event("response.output_text.delta", map[string]any{
						"type":          "response.output_text.delta",
						"item_id":       messageID,
						"output_index":  messageIndex,
						"content_index": 0,
						"delta":         c,
					})
				}
				// 推理
				r, _ := delta["reasoning_content"].(string)
				if r == "" {
					r, _ = delta["reasoning"].(string)
				}
				if r != "" {
					if !reasoningEmitted {
						reasoningEmitted = true
						reasoningIndex = allocIndex()
						s.event("response.output_item.added", map[string]any{
							"type":         "response.output_item.added",
							"output_index": reasoningIndex,
							"item": map[string]any{
								"id":                reasoningID,
								"type":              "reasoning",
								"status":            "in_progress",
								"summary":           []any{},
								"encrypted_content": nil,
							},
						})
						s.event("response.reasoning_summary_part.added", map[string]any{
							"type":          "response.reasoning_summary_part.added",
							"item_id":       reasoningID,
							"output_index":  reasoningIndex,
							"summary_index": 0,
							"part": map[string]any{
								"type": "summary_text",
								"text": "",
							},
						})
					}
					reasoningText.WriteString(r)
					s.event("response.reasoning_summary_text.delta", map[string]any{
						"type":          "response.reasoning_summary_text.delta",
						"item_id":       reasoningID,
						"output_index":  reasoningIndex,
						"summary_index": 0,
						"content_index": 0,
						"delta":         r,
					})
				}
				// 工具调用：按 index / call_id 分桶，避免并行调用的 arguments 被拼接
				if tc, ok := delta["tool_calls"].([]any); ok {
					for _, c := range tc {
						cm, ok := c.(map[string]any)
						if !ok {
							continue
						}
						var idxPtr *int
						if raw, ok := cm["index"].(float64); ok {
							i := int(raw)
							idxPtr = &i
						}
						id, _ := cm["id"].(string)
						var name, args string
						hasName := false
						if fn, ok := cm["function"].(map[string]any); ok {
							if n, ok := fn["name"].(string); ok && n != "" {
								name, hasName = n, true
							}
							if a, ok := fn["arguments"].(string); ok {
								args = a
							}
						}
						acc := calls.slot(idxPtr, id, name, hasName)
						if hasName {
							acc.name = name
						}
						if id != "" {
							calls.bindID(acc, id)
						}
						if args != "" {
							acc.args.WriteString(args)
						}
						emitCall(acc)
					}
				}
			}
		}
		if err != nil {
			break
		}
	}

	// 收尾
	closeReasoning()
	closeText()
	outputItems := []any{}
	if reasoningEmitted {
		outputItems = append(outputItems, reasoningItem(reasoningID, reasoningText.String()))
	}
	if textEmitted {
		outputItems = append(outputItems, messageItem(messageID, outText.String()))
	}
	for _, acc := range calls.all() {
		emitCall(acc)
		if !acc.emitted {
			// 没有 name 的残缺调用无法映射为 function_call，跳过而不是拼成非法 JSON
			continue
		}
		s.event("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": acc.outputID, "output_index": acc.outIndex, "arguments": acc.args.String()})
		s.event("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": acc.outIndex, "item": map[string]any{"type": "function_call", "id": acc.outputID, "call_id": acc.callID, "name": acc.name, "arguments": acc.args.String(), "status": "completed"}})
		outputItems = append(outputItems, map[string]any{"type": "function_call", "id": acc.outputID, "call_id": acc.callID, "name": acc.name, "arguments": acc.args.String(), "status": "completed"})
	}

	callArgs := make([]string, 0, len(calls.all()))
	for _, acc := range calls.all() {
		if acc.args.Len() > 0 {
			callArgs = append(callArgs, acc.args.String())
		}
	}
	u := usage.finish(fallbackInputTokens, outText.String(), reasoningText.String(), callArgs...)
	if u.estimated {
		log.Printf("  responses usage: upstream not provided, estimated input=%d output=%d", u.inputTokens, u.outputTokens)
	}
	s.event("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":          s.respID,
			"object":      "response",
			"created_at":  time.Now().Unix(),
			"status":      "completed",
			"model":       model,
			"output":      outputItems,
			"output_text": outText.String(),
			"usage": map[string]any{
				"input_tokens": u.inputTokens,
				"input_tokens_details": map[string]any{
					"cached_tokens": u.cachedTokens,
				},
				"output_tokens": u.outputTokens,
				"output_tokens_details": map[string]any{
					"reasoning_tokens": u.reasoningTokens,
				},
				"total_tokens": u.totalTokens,
			},
		},
	})
}

// reasoningItem 构造 reasoning output item（added/done 与 completed.output 复用）。
func reasoningItem(id, text string) map[string]any {
	return map[string]any{
		"id":     id,
		"type":   "reasoning",
		"status": "completed",
		"summary": []any{map[string]any{
			"type": "summary_text",
			"text": text,
		}},
	}
}

// messageItem 构造 assistant message output item。
func messageItem(id, text string) map[string]any {
	return map[string]any{
		"id":     id,
		"type":   "message",
		"role":   "assistant",
		"status": "completed",
		"content": []any{map[string]any{
			"type":        "output_text",
			"text":        text,
			"annotations": []any{},
		}},
	}
}

// ============ /v1/responses 入口 ============

func handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	var params map[string]any
	if err := json.Unmarshal(body, &params); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	model, _ := params["model"].(string)
	isStream, _ := params["stream"].(bool)
	log.Printf("  responses: model=%s stream=%v", model, isStream)

	chat := responsesToChat(params)
	chatModel, _ := chat["model"].(string)
	route := routeModel(chatModel)
	if route == "reject" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": fmt.Sprintf("model %q is a paid zen model; only free zen models are proxied", chatModel), "type": "invalid_request_error"},
		})
		return
	}
	if route == "zen" {
		zm, ok := resolveZenFreeModel(chatModel)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": fmt.Sprintf("model %q is not a free zen model", chatModel), "type": "invalid_request_error"},
			})
			return
		}
		sid := requestSessionID(chat, r.Header)
		out := maybeCompact(chat, zm, sid)
		if out.changed {
			log.Printf("  responses zen: %s", out.note)
		}
		resp, _, err := callZenAPI(chat, isStream)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "api_error"},
			})
			return
		}
		defer resp.Body.Close()
		if isStream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.WriteHeader(http.StatusOK)
			chatStreamToResponses(w, resp, nil, estimateJSON(chat), chatModel)
			return
		}
		var raw map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, chatToResponses(raw, estimateJSON(chat)))
		return
	}

	// cline 上游
	stream := isStream
	if !isStream && modelNeedsStream(normalizeRequestModel(chatModel)) {
		stream = true
	}
	up, acc, err := callClineAPI(chat, stream)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		return
	}
	defer up.Body.Close()

	usageFn := accountUsageFn(acc, chat)
	usageFnWithLog := func(u map[string]any) {
		setRequestLogMetaFromUsage(r, acc, u, chat)
		usageFn(u)
	}
	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
		chatStreamToResponses(w, up, usageFnWithLog, estimateJSON(chat), chatModel)
		return
	}
	if stream {
		out, err := collectStreamResponse(up)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if u, ok := out["usage"].(map[string]any); ok && len(u) > 0 {
			usageFnWithLog(u)
		}
		writeJSON(w, http.StatusOK, chatToResponses(out, estimateJSON(chat)))
		return
	}
	var raw map[string]any
	if err := json.NewDecoder(up.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if u, ok := raw["usage"].(map[string]any); ok && len(u) > 0 {
		usageFnWithLog(u)
	}
	out := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			out = d
		}
	}
	writeJSON(w, http.StatusOK, chatToResponses(out, estimateJSON(chat)))
}
