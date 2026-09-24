package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// collectCompleted 跑完一次流式转换，返回全部事件。
func collectCompleted(t *testing.T, payloads ...string) []map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	chatStreamToResponses(recorder, chatSSE(payloads...), nil, 1234, "test-model")
	return decodeResponseEvents(t, recorder.Body.String())
}

// toolArgsDone 提取所有 function_call 的收尾 arguments。
func toolArgsDone(t *testing.T, events []map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range events {
		if e["type"] == "response.output_item.done" {
			item, _ := e["item"].(map[string]any)
			if item != nil && item["type"] == "function_call" {
				out = append(out, e)
			}
		}
	}
	return out
}

// P0: 同一 delta 里出现两个并行调用，必须各自独立成 item，arguments 不能拼接。
func TestChatStreamToResponsesSplitsParallelToolCalls(t *testing.T) {
	events := collectCompleted(t,
		`{"model":"m","choices":[{"delta":{"tool_calls":[`+
			`{"index":0,"id":"call_a","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}},`+
			`{"index":1,"id":"call_b","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"}}`+
			`]}}]}`,
	)
	done := toolArgsDone(t, events)
	if len(done) != 2 {
		t.Fatalf("function_call items = %d, want 2", len(done))
	}
	seenID := map[string]bool{}
	seenIndex := map[int]bool{}
	for _, e := range done {
		item := e["item"].(map[string]any)
		args, _ := item["arguments"].(string)
		var parsed map[string]any
		if err := json.Unmarshal([]byte(args), &parsed); err != nil {
			t.Fatalf("arguments %q is not valid JSON: %v", args, err)
		}
		id, _ := item["call_id"].(string)
		if seenID[id] {
			t.Fatalf("duplicate call_id %q", id)
		}
		seenID[id] = true
		idx := int(e["output_index"].(float64))
		if seenIndex[idx] {
			t.Fatalf("duplicate output_index %d", idx)
		}
		seenIndex[idx] = true
	}
	if !seenID["call_a"] || !seenID["call_b"] {
		t.Fatalf("call ids not preserved: %#v", seenID)
	}
}

// P0: 参数跨多个 delta 分段到达，必须累积到同一个调用。
func TestChatStreamToResponsesAccumulatesArgsAcrossDeltas(t *testing.T) {
	events := collectCompleted(t,
		`{"model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"exec_command","arguments":"{\"cmd\":"}}]}}]}`,
		`{"model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls -la\"}"}}]}}]}`,
	)
	done := toolArgsDone(t, events)
	if len(done) != 1 {
		t.Fatalf("function_call items = %d, want 1", len(done))
	}
	args := done[0]["item"].(map[string]any)["arguments"].(string)
	if args != `{"cmd":"ls -la"}` {
		t.Fatalf("arguments = %q", args)
	}
}

// P0: 上游不给 index，只有 call_id 时也要按 call_id 分桶。
func TestChatStreamToResponsesBucketsByCallIDWithoutIndex(t *testing.T) {
	events := collectCompleted(t,
		`{"model":"m","choices":[{"delta":{"tool_calls":[{"id":"call_a","function":{"name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
		`{"model":"m","choices":[{"delta":{"tool_calls":[{"id":"call_b","function":{"name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"}}]}}]}`,
	)
	done := toolArgsDone(t, events)
	if len(done) != 2 {
		t.Fatalf("function_call items = %d, want 2", len(done))
	}
	for _, e := range done {
		args := e["item"].(map[string]any)["arguments"].(string)
		var parsed map[string]any
		if err := json.Unmarshal([]byte(args), &parsed); err != nil {
			t.Fatalf("arguments %q is not valid JSON: %v", args, err)
		}
	}
}

// P0: reasoning + 正文 + 工具调用同轮出现时，output_index 必须唯一。
func TestChatStreamToResponsesAllocatesUniqueOutputIndexes(t *testing.T) {
	events := collectCompleted(t,
		`{"model":"m","choices":[{"delta":{"reasoning_content":"想想"}}]}`,
		`{"model":"m","choices":[{"delta":{"content":"好"}}]}`,
		`{"model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"exec_command","arguments":"{}"}}]}}]}`,
	)
	seen := map[int]bool{}
	for _, e := range events {
		if e["type"] != "response.output_item.added" {
			continue
		}
		idx := int(e["output_index"].(float64))
		if seen[idx] {
			t.Fatalf("output_index %d reused", idx)
		}
		seen[idx] = true
	}
	if len(seen) != 3 {
		t.Fatalf("distinct output_index = %d, want 3", len(seen))
	}
}

// P1: 上游下发 usage 时，response.completed 必须回填真实数值。
func TestChatStreamToResponsesPropagatesUpstreamUsage(t *testing.T) {
	events := collectCompleted(t,
		`{"model":"m","choices":[{"delta":{"content":"你好"}}],"usage":{"prompt_tokens":1200,"completion_tokens":34,"total_tokens":1234,"reasoning_tokens":7,"prompt_tokens_details":{"cached_tokens":900}}}`,
	)
	completed := findResponseEvent(t, events, "response.completed", nil)
	response := completed["response"].(map[string]any)
	usage := response["usage"].(map[string]any)
	if got := usage["input_tokens"]; got != float64(1200) {
		t.Fatalf("input_tokens = %v, want 1200", got)
	}
	if got := usage["output_tokens"]; got != float64(34) {
		t.Fatalf("output_tokens = %v, want 34", got)
	}
	if got := usage["total_tokens"]; got != float64(1234) {
		t.Fatalf("total_tokens = %v, want 1234", got)
	}
	details := usage["input_tokens_details"].(map[string]any)
	if got := details["cached_tokens"]; got != float64(900) {
		t.Fatalf("cached_tokens = %v, want 900", got)
	}
	outDetails := usage["output_tokens_details"].(map[string]any)
	if got := outDetails["reasoning_tokens"]; got != float64(7) {
		t.Fatalf("reasoning_tokens = %v, want 7", got)
	}
}

// P1: 上游不给 usage 时必须估算兜底，绝不能返回 0。
func TestChatStreamToResponsesEstimatesUsageWhenUpstreamOmitsIt(t *testing.T) {
	events := collectCompleted(t,
		`{"model":"m","choices":[{"delta":{"content":"这是一段中文回答，用来触发估算。"}}]}`,
	)
	completed := findResponseEvent(t, events, "response.completed", nil)
	usage := completed["response"].(map[string]any)["usage"].(map[string]any)
	if got := usage["input_tokens"]; got != float64(1234) {
		t.Fatalf("input_tokens = %v, want fallback 1234", got)
	}
	output := usage["output_tokens"].(float64)
	if output <= 0 {
		t.Fatalf("output_tokens = %v, want > 0", output)
	}
	if got := usage["total_tokens"]; got != float64(1234)+output {
		t.Fatalf("total_tokens = %v, want input+output", got)
	}
}

// P1: completed 事件要带上 output 数组，便于客户端重建消息。
func TestChatStreamToResponsesIncludesOutputItems(t *testing.T) {
	events := collectCompleted(t,
		`{"model":"m","choices":[{"delta":{"content":"hi"}}]}`,
		`{"model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"exec_command","arguments":"{}"}}]}}]}`,
	)
	completed := findResponseEvent(t, events, "response.completed", nil)
	response := completed["response"].(map[string]any)
	output, ok := response["output"].([]any)
	if !ok {
		t.Fatalf("output is not an array: %#v", response["output"])
	}
	if len(output) != 2 {
		t.Fatalf("output items = %d, want message + function_call", len(output))
	}
}

// P1: 转换出的 chat 请求必须显式索取 usage。
func TestResponsesToChatRequestsIncludeUsageWhenStreaming(t *testing.T) {
	chat := responsesToChat(map[string]any{
		"model":  "test-model",
		"stream": true,
		"input":  "hi",
	})
	opts, ok := chat["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options missing: %#v", chat["stream_options"])
	}
	if opts["include_usage"] != true {
		t.Fatalf("include_usage = %v, want true", opts["include_usage"])
	}
}

// P0: 首个片段只带 index（无 id / name），后续片段才补齐 id 与 name，
// 整个调用必须落在同一个 item 上。
func TestChatStreamToResponsesBindsLateCallID(t *testing.T) {
	events := collectCompleted(t,
		`{"model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":"}}]}}]}`,
		`{"model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_late","function":{"name":"exec_command","arguments":"\"ls\"}"}}]}}]}`,
	)
	done := toolArgsDone(t, events)
	if len(done) != 1 {
		t.Fatalf("function_call items = %d, want 1", len(done))
	}
	item := done[0]["item"].(map[string]any)
	if item["call_id"] != "call_late" {
		t.Fatalf("call_id = %v, want call_late", item["call_id"])
	}
	if item["name"] != "exec_command" {
		t.Fatalf("name = %v", item["name"])
	}
	if args := item["arguments"].(string); args != `{"cmd":"ls"}` {
		t.Fatalf("arguments = %q", args)
	}
}

// P0: name 缺失的残缺调用不能伪造出 function_call。
func TestChatStreamToResponsesSkipsNamelessCall(t *testing.T) {
	events := collectCompleted(t,
		`{"model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","function":{"arguments":"{}"}}]}}]}`,
	)
	if done := toolArgsDone(t, events); len(done) != 0 {
		t.Fatalf("function_call items = %d, want 0", len(done))
	}
}

// P0 回归: 取自真实会话中曾被拼坏的两条 exec_command（2026-09-23 rollout
// 01a0ca1d-d3d）。修复前 arguments 被拼成 `{...}{...}`，Codex 报
// "failed to parse function arguments: trailing characters"。
func TestChatStreamToResponsesRealWorldDoubleExecRegression(t *testing.T) {
	cmd1 := `{"cmd": "pwd && printf '\\n-- top-level --\\n' && ls -la", "justification": "", "login": true, "max_output_tokens": 12000, "workdir": "/Users/dixin/workspace-dubhe", "yield_time_ms": 10000}`
	cmd2 := `{"cmd": "for f in CLAUDE.md ROADMAP.md AGENTS.md; do if [ -f \"$f\" ]; then echo \"===== $f =====\"; sed -n '1,240p' \"$f\"; fi; done", "justification": "", "login": true, "max_output_tokens": 20000, "workdir": "/Users/dixin/workspace-dubhe", "yield_time_ms": 10000}`

	// 分片模拟真实流式：每条命令的 arguments 拆成两段。
	half1a, half1b := cmd1[:40], cmd1[40:]
	half2a, half2b := cmd2[:30], cmd2[30:]
	delta := func(index int, id, name, args string) string {
		return `{"model":"m","choices":[{"delta":{"tool_calls":[{"index":` + itoa(int64(index)) +
			`,"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":` + jsonString(args) + `}}]}}]}`
	}
	events := collectCompleted(t,
		delta(0, "call_01_q8Fb6ZRRjoJR4tm47SUU3503", "exec_command", half1a),
		delta(1, "call_02_AbCdEfGhIjKlMnOpQrStUvWx", "exec_command", half2a),
		delta(0, "", "", half1b),
		delta(1, "", "", half2b),
	)

	done := toolArgsDone(t, events)
	if len(done) != 2 {
		t.Fatalf("function_call items = %d, want 2", len(done))
	}
	want := map[string]string{"call_01_q8Fb6ZRRjoJR4tm47SUU3503": cmd1, "call_02_AbCdEfGhIjKlMnOpQrStUvWx": cmd2}
	for _, e := range done {
		item := e["item"].(map[string]any)
		id := item["call_id"].(string)
		args := item["arguments"].(string)
		if want[id] != args {
			t.Fatalf("call %s arguments mismatch:\n got: %s\nwant: %s", id, args, want[id])
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(args), &parsed); err != nil {
			t.Fatalf("call %s arguments is not valid JSON: %v", id, err)
		}
	}
}

// jsonString 用于在测试里把 Go 字符串安全地嵌进 JSON 文本。
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// P1 回归：非流式 Responses 上游缺失 usage 时同样必须估算，不能回传全 0。
func TestChatToResponsesEstimatesMissingUsage(t *testing.T) {
	resp := chatToResponses(map[string]any{
		"model": "test-model",
		"choices": []any{map[string]any{
			"message": map[string]any{
				"content":           "这是非流式回答。",
				"reasoning_content": "先判断请求类型。",
				"tool_calls": []any{map[string]any{
					"id": "call_a",
					"function": map[string]any{
						"name":      "exec_command",
						"arguments": `{"cmd":"pwd"}`,
					},
				}},
			},
		}},
	}, 456)
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"].(int) != 456 {
		t.Fatalf("input_tokens = %v, want 456", usage["input_tokens"])
	}
	if usage["output_tokens"].(int) <= 0 {
		t.Fatalf("output_tokens = %v, want > 0", usage["output_tokens"])
	}
	if usage["total_tokens"].(int) != usage["input_tokens"].(int)+usage["output_tokens"].(int) {
		t.Fatalf("total_tokens = %v, want input+output", usage["total_tokens"])
	}
}

func TestChatStreamToResponsesFailsOnUnexpectedEOF(t *testing.T) {
	recorder := httptest.NewRecorder()
	upstream := &http.Response{Body: io.NopCloser(strings.NewReader(
		`data: {"model":"m","choices":[{"delta":{"content":"partial"}}]}` + "\n\n",
	))}
	chatStreamToResponses(recorder, upstream, nil, 10, "m")
	events := decodeResponseEvents(t, recorder.Body.String())

	failed := findResponseEvent(t, events, "response.failed", nil)
	response := failed["response"].(map[string]any)
	if response["status"] != "failed" {
		t.Fatalf("response.status = %v, want failed", response["status"])
	}
	for _, event := range events {
		if event["type"] == "response.completed" {
			t.Fatal("unexpected response.completed after EOF without [DONE]")
		}
	}
}

type heartbeatBody struct {
	mu       sync.Mutex
	first    bool
	released chan struct{}
	once     sync.Once
}

func (b *heartbeatBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	first := b.first
	if !first {
		b.first = true
	}
	b.mu.Unlock()
	if !first {
		return copy(p, "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"), nil
	}
	<-b.released
	return 0, io.EOF
}

func (b *heartbeatBody) Close() error {
	b.once.Do(func() { close(b.released) })
	return nil
}

func TestChatStreamToResponsesSendsHeartbeatWhileUpstreamIsIdle(t *testing.T) {
	oldHeartbeat := streamHeartbeatInterval
	oldIdle := streamReadIdleTimeout
	t.Cleanup(func() {
		streamHeartbeatInterval = oldHeartbeat
		streamReadIdleTimeout = oldIdle
	})
	streamHeartbeatInterval = 5 * time.Millisecond
	streamReadIdleTimeout = 25 * time.Millisecond

	recorder := httptest.NewRecorder()
	body := &heartbeatBody{released: make(chan struct{})}
	chatStreamToResponses(recorder, &http.Response{Body: body}, nil, 10, "m")
	if !strings.Contains(recorder.Body.String(), ": keep-alive\n\n") {
		t.Fatalf("stream did not contain heartbeat: %q", recorder.Body.String())
	}
	events := decodeResponseEvents(t, recorder.Body.String())
	findResponseEvent(t, events, "response.failed", nil)
}
