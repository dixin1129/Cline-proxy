package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cline-go-proxy/internal/kit"
)

// setupResponsesE2E 隔离账号池与上游 HTTP 客户端，让 handleResponses 走到假上游。
// upstream 为假上游响应体；返回恢复函数。
func setupResponsesE2E(t *testing.T, upstreamBody string) {
	t.Helper()
	oldClient := kit.HTTPClient
	oldPool := pool
	oldPoolPath := poolPath
	t.Cleanup(func() {
		kit.HTTPClient = oldClient
		pool = oldPool
		poolPath = oldPoolPath
	})

	poolPath = t.TempDir() + "/accounts.json"
	pool = &AccountPool{Accounts: []*Account{{
		AccountID:   "e2e",
		Email:       "e2e@example.com",
		AccessToken: "token",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}}}
	kit.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       newSlowBody(upstreamBody),
		}, nil
	})}
}

// slowBody 模拟逐行到达的 SSE 上游（每行之间留 1ms，验证流式增量处理）。
type slowBody struct {
	lines []string
	idx   int
	buf   strings.Reader
}

func newSlowBody(payload string) *slowBody {
	lines := strings.SplitAfter(payload, "\n")
	return &slowBody{lines: lines}
}

func (b *slowBody) Read(p []byte) (int, error) {
	if b.buf.Len() == 0 {
		if b.idx >= len(b.lines) {
			return 0, io.EOF
		}
		b.buf = *strings.NewReader(b.lines[b.idx])
		b.idx++
		time.Sleep(time.Millisecond)
	}
	return b.buf.Read(p)
}

func (b *slowBody) Close() error { return nil }

// E2E: 一轮响应里包含 reasoning + 正文 + 两个并行工具调用，
// 断言 Codex 侧能解析出两个独立且合法的 function_call。
func TestHandleResponsesParallelToolCallsEndToEnd(t *testing.T) {
	setupResponsesE2E(t, `data: {"model":"deepseek/deepseek-v4-flash","choices":[{"delta":{"reasoning_content":"我需要列目录"}}]}
data: {"model":"deepseek/deepseek-v4-flash","choices":[{"delta":{"content":"开始执行。"}}]}
data: {"model":"deepseek/deepseek-v4-flash","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}}]}}]}
data: {"model":"deepseek/deepseek-v4-flash","choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"}}]}}]}
data: {"model":"deepseek/deepseek-v4-flash","choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4321,"completion_tokens":77,"total_tokens":4398,"reasoning_tokens":12}}
data: [DONE]

`)

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{
		"model":"deepseek/deepseek-v4-flash",
		"stream":true,
		"input":"帮我看看目录"
	}`))
	recorder := httptest.NewRecorder()
	handleResponses(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	events := decodeResponseEvents(t, recorder.Body.String())

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

	completed := findResponseEvent(t, events, "response.completed", nil)
	usage := completed["response"].(map[string]any)["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4321) || usage["output_tokens"] != float64(77) {
		t.Fatalf("usage not propagated: %#v", usage)
	}
	if completed["response"].(map[string]any)["model"] != "deepseek/deepseek-v4-flash" {
		t.Fatalf("model = %v", completed["response"].(map[string]any)["model"])
	}
}

// E2E: 上游完全不给 usage 时，completed 仍要给出非 0 的估算值。
func TestHandleResponsesEstimatesUsageEndToEnd(t *testing.T) {
	setupResponsesE2E(t, `data: {"model":"deepseek/deepseek-v4-flash","choices":[{"delta":{"content":"这是一段用于估算的中文回答。"}}]}
data: [DONE]

`)
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{
		"model":"deepseek/deepseek-v4-flash",
		"stream":true,
		"input":"你好"
	}`))
	recorder := httptest.NewRecorder()
	handleResponses(recorder, req)

	events := decodeResponseEvents(t, recorder.Body.String())
	completed := findResponseEvent(t, events, "response.completed", nil)
	usage := completed["response"].(map[string]any)["usage"].(map[string]any)
	if usage["input_tokens"].(float64) <= 0 {
		t.Fatalf("input_tokens = %v, want > 0", usage["input_tokens"])
	}
	if usage["output_tokens"].(float64) <= 0 {
		t.Fatalf("output_tokens = %v, want > 0", usage["output_tokens"])
	}
}
