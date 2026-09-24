package app

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatStreamToResponsesEmitsCodexCompatibleReasoningItem(t *testing.T) {
	upstream := chatSSE(`{"model":"z-ai/glm-5.3-flash","choices":[{"delta":{"reasoning_content":"先计算"}}]}`,
		`{"model":"z-ai/glm-5.3-flash","choices":[{"delta":{"content":"408"}}]}`)
	recorder := httptest.NewRecorder()

	chatStreamToResponses(recorder, upstream, nil, 0, "z-ai/glm-5.3-flash")
	events := decodeResponseEvents(t, recorder.Body.String())

	reasoningAdded := findResponseEvent(t, events, "response.output_item.added", func(event map[string]any) bool {
		item, _ := event["item"].(map[string]any)
		return item["type"] == "reasoning"
	})
	item := reasoningAdded["item"].(map[string]any)
	if _, ok := item["summary"].([]any); !ok {
		t.Fatalf("reasoning item must include summary array: %#v", item)
	}
	if item["encrypted_content"] != nil {
		t.Fatalf("reasoning item encrypted_content = %#v, want nil", item["encrypted_content"])
	}

	messageAdded := findResponseEvent(t, events, "response.output_item.added", func(event map[string]any) bool {
		item, _ := event["item"].(map[string]any)
		return item["type"] == "message"
	})
	if got := int(messageAdded["output_index"].(float64)); got != 1 {
		t.Fatalf("message output_index = %d, want 1 after reasoning", got)
	}
}

func TestChatStreamToResponsesUsesZeroIndexWithoutReasoningAndValidUsage(t *testing.T) {
	upstream := chatSSE(`{"model":"z-ai/glm-5.3-flash","choices":[{"delta":{"content":"408"}}]}`)
	recorder := httptest.NewRecorder()

	chatStreamToResponses(recorder, upstream, nil, 0, "z-ai/glm-5.3-flash")
	events := decodeResponseEvents(t, recorder.Body.String())

	messageAdded := findResponseEvent(t, events, "response.output_item.added", func(event map[string]any) bool {
		item, _ := event["item"].(map[string]any)
		return item["type"] == "message"
	})
	if got := int(messageAdded["output_index"].(float64)); got != 0 {
		t.Fatalf("message output_index = %d, want 0 without reasoning", got)
	}

	completed := findResponseEvent(t, events, "response.completed", nil)
	response := completed["response"].(map[string]any)
	usage := response["usage"].(map[string]any)
	if _, ok := usage["input_tokens_details"]; !ok {
		t.Fatal("response.completed usage is missing input_tokens_details")
	}
	if _, ok := usage["output_tokens_details"]; !ok {
		t.Fatal("response.completed usage is missing output_tokens_details")
	}
}

func chatSSE(payloads ...string) *http.Response {
	var body strings.Builder
	for _, payload := range payloads {
		body.WriteString("data: ")
		body.WriteString(payload)
		body.WriteString("\n\n")
	}
	body.WriteString("data: [DONE]\n\n")
	return &http.Response{Body: io.NopCloser(strings.NewReader(body.String()))}
}

func decodeResponseEvents(t *testing.T, stream string) []map[string]any {
	t.Helper()
	var events []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(stream))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan events: %v", err)
	}
	return events
}

func findResponseEvent(t *testing.T, events []map[string]any, eventType string, match func(map[string]any) bool) map[string]any {
	t.Helper()
	for _, event := range events {
		if event["type"] == eventType && (match == nil || match(event)) {
			return event
		}
	}
	t.Fatalf("event %q not found", eventType)
	return nil
}
