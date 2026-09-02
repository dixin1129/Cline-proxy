package app

import (
	"cline-go-proxy/internal/kit"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestValidateProbeResponse(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		valid       bool
		rateLimited bool
	}{
		{
			name:  "non-stream assistant message",
			body:  `{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`,
			valid: true,
		},
		{
			name:  "wrapped non-stream assistant message",
			body:  `{"data":{"choices":[{"message":{"content":"pong"}}]}}`,
			valid: true,
		},
		{
			name: "stream assistant content after reasoning",
			body: "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\n" +
				"data: [DONE]\n\n",
			valid: true,
		},
		{
			name:  "tool call",
			body:  `{"choices":[{"message":{"tool_calls":[{"id":"call_1","type":"function"}]}}]}`,
			valid: true,
		},
		{
			name: "reasoning only is not a reply",
			body: "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "done only is not a reply",
			body: "data: [DONE]\n\n",
		},
		{
			name: "empty response is not a reply",
			body: "   \n",
		},
		{
			name:        "embedded rate limit error",
			body:        `data: {"error":{"code":"INFERENCE_CAP_ERROR","message":"Try again in 2h"}}` + "\n\n",
			rateLimited: true,
		},
		{
			name: "embedded generic error",
			body: `data: {"error":{"message":"upstream failed"}}` + "\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateProbeResponse([]byte(tt.body))
			if got.valid != tt.valid {
				t.Fatalf("valid = %v, want %v; result=%+v", got.valid, tt.valid, got)
			}
			if got.rateLimited != tt.rateLimited {
				t.Fatalf("rateLimited = %v, want %v; result=%+v", got.rateLimited, tt.rateLimited, got)
			}
		})
	}
}

func TestTestAccountDoesNotActivateOnHTTP200WithoutReply(t *testing.T) {
	oldClient := kit.HTTPClient
	oldPool := pool
	oldPoolPath := poolPath
	t.Cleanup(func() {
		kit.HTTPClient = oldClient
		pool = oldPool
		poolPath = oldPoolPath
	})

	poolPath = t.TempDir() + "/accounts.json"
	acc := &Account{
		AccountID:     "test-account",
		Email:         "test@example.com",
		AccessToken:   "access-token",
		ExpiresAt:     time.Now().Add(time.Hour).UnixMilli(),
		Status:        "cooldown",
		CooldownUntil: time.Now().Add(time.Hour),
	}
	pool = &AccountPool{Accounts: []*Account{acc}}
	kit.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\ndata: [DONE]\n\n")),
		}, nil
	})}

	result, status := testAccount(acc)
	if status != "error" {
		t.Fatalf("status = %q, want error; result=%+v", status, result)
	}
	if acc.Status != "cooldown" {
		t.Fatalf("account status = %q, want cooldown", acc.Status)
	}
	if result["probeValid"] != false {
		t.Fatalf("probeValid = %#v, want false; result=%+v", result["probeValid"], result)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
