package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cline-go-proxy/internal/kit"
)

func TestCallClineAPIRotatesAccountOnEmbeddedRateLimit(t *testing.T) {
	oldClient := kit.HTTPClient
	oldPool := pool
	oldPoolPath := poolPath
	oldDelay := clineRetryDelay
	t.Cleanup(func() {
		kit.HTTPClient = oldClient
		pool = oldPool
		poolPath = oldPoolPath
		clineRetryDelay = oldDelay
	})

	clineRetryDelay = 0
	poolPath = t.TempDir() + "/accounts.json"
	first := &Account{AccountID: "a1", Email: "one@example.com", AccessToken: "token-1", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active"}
	second := &Account{AccountID: "a2", Email: "two@example.com", AccessToken: "token-2", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active"}
	pool = &AccountPool{Accounts: []*Account{first, second}}

	requests := 0
	kit.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"error":{"code":"INFERENCE_CAP_ERROR","message":"Daily free limit reached. Try again in 2h"}}`,
				)),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`)),
		}, nil
	})}

	resp, acc, err := callClineAPIContext(context.Background(), map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, false)
	if err != nil {
		t.Fatalf("callClineAPIContext() error = %v", err)
	}
	defer resp.Body.Close()
	if requests != 2 {
		t.Fatalf("upstream requests = %d, want 2", requests)
	}
	if acc == nil || acc.Status != "active" {
		t.Fatalf("successful account = %#v, want active", acc)
	}
	if first.Status != "cooldown" && second.Status != "cooldown" {
		t.Fatalf("accounts after rate limit = %#v / %#v, want one cooldown", first.Status, second.Status)
	}
	if first.Status == "cooldown" && second.Status == "cooldown" {
		t.Fatal("both accounts entered cooldown after one rate-limited request")
	}
}

func TestCallClineAPIRetriesNetworkFailureThreeTotalAttempts(t *testing.T) {
	oldClient := kit.HTTPClient
	oldPool := pool
	oldPoolPath := poolPath
	oldDelay := clineRetryDelay
	t.Cleanup(func() {
		kit.HTTPClient = oldClient
		pool = oldPool
		poolPath = oldPoolPath
		clineRetryDelay = oldDelay
	})

	clineRetryDelay = 0
	poolPath = t.TempDir() + "/accounts.json"
	acc := &Account{AccountID: "network", Email: "network@example.com", AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active"}
	pool = &AccountPool{Accounts: []*Account{acc}}

	requests := 0
	kit.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		if requests < 3 {
			return nil, errors.New("temporary dial failure")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`)),
		}, nil
	})}

	resp, gotAcc, err := callClineAPIContext(context.Background(), map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, false)
	if err != nil {
		t.Fatalf("callClineAPIContext() error = %v", err)
	}
	defer resp.Body.Close()
	if requests != clineMaxAttempts {
		t.Fatalf("upstream requests = %d, want %d", requests, clineMaxAttempts)
	}
	if gotAcc != acc {
		t.Fatalf("successful account pointer = %p, want %p", gotAcc, acc)
	}
	if acc.Status != "active" {
		t.Fatalf("account status = %q, want active after transient recovery", acc.Status)
	}
	if acc.UsageCount != 1 {
		t.Fatalf("usage count = %d, want 1", acc.UsageCount)
	}
}

func TestWriteClineAPIErrorPreservesUpstreamStatus(t *testing.T) {
	for _, want := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(want), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeClineAPIError(recorder, &clineAPIError{StatusCode: want, Message: "upstream failed"})
			if recorder.Code != want {
				t.Fatalf("status = %d, want %d", recorder.Code, want)
			}
		})
	}
}
