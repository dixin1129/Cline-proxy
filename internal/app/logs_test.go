package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequestLogMiddlewareRecordsDuration(t *testing.T) {
	reqLogsMu.Lock()
	oldLogs := reqLogs
	reqLogs = nil
	reqLogsMu.Unlock()
	oldLogsFile := reqLogsFile
	reqLogsFile = t.TempDir() + "/requests.jsonl"
	t.Cleanup(func() {
		reqLogsMu.Lock()
		reqLogs = oldLogs
		reqLogsMu.Unlock()
		reqLogsFile = oldLogsFile
	})

	handler := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta, ok := r.Context().Value(apiKeyAuthMetaKey{}).(*apiKeyAuthMeta)
		if !ok {
			t.Fatal("request log auth metadata is missing")
		}
		meta.authed = true
		time.Sleep(10 * time.Millisecond)
		io.WriteString(w, "ok")
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model"}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	logs := LoadRequestLogs()
	if len(logs) != 1 {
		t.Fatalf("request log count = %d, want 1", len(logs))
	}
	if logs[0].Duration < 10 {
		t.Fatalf("request duration = %d ms, want at least 10 ms", logs[0].Duration)
	}
}
