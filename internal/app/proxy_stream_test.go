package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleStreamResponseWithUsageFailsOnUnexpectedEOF(t *testing.T) {
	recorder := httptest.NewRecorder()
	upstream := &http.Response{Body: io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n",
	))}
	handleStreamResponseWithUsageContext(context.Background(), recorder, upstream, nil)

	body := recorder.Body.String()
	if strings.Contains(body, "data: [DONE]") {
		t.Fatal("unexpected [DONE] after upstream EOF")
	}
	if !strings.Contains(body, `"type":"api_error"`) {
		t.Fatalf("missing streamed error: %q", body)
	}
}
