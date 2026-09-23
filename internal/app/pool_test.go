package app

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"cline-go-proxy/internal/kit"
)

func TestAccountUsageCountPersistsAndResetsDaily(t *testing.T) {
	oldPool := pool
	oldPoolPath := poolPath
	t.Cleanup(func() {
		pool = oldPool
		poolPath = oldPoolPath
	})

	poolPath = t.TempDir() + "/accounts.json"
	today := time.Now().Format("2006-01-02")
	account := &Account{
		AccountID:       "usage-test",
		Email:           "usage@example.com",
		Status:          "active",
		UsageCount:      7,
		UsageCountToday: 3,
		UsageDate:       today,
	}
	pool = &AccountPool{Accounts: []*Account{account}}

	bumpUsage(account)
	if account.UsageCountToday != 4 || account.UsageCount != 8 {
		t.Fatalf("usage count = %d today / %d total, want 4 today / 8 total", account.UsageCountToday, account.UsageCount)
	}

	data, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatalf("read persisted pool: %v", err)
	}
	var persisted AccountPool
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("decode persisted pool: %v", err)
	}
	if len(persisted.Accounts) != 1 || persisted.Accounts[0].UsageCountToday != 4 || persisted.Accounts[0].UsageCount != 8 {
		t.Fatalf("persisted usage count = %+v, want 4 today / 8 total", persisted.Accounts[0])
	}

	account.UsageDate = "2000-01-01"
	bumpUsage(account)
	if account.UsageCountToday != 1 || account.UsageCount != 9 {
		t.Fatalf("after date rollover usage count = %d today / %d total, want 1 today / 9 total", account.UsageCountToday, account.UsageCount)
	}

	listed := ListAccounts()
	if len(listed) != 1 || listed[0].UsageCountToday != 1 || listed[0].UsageCount != 9 {
		t.Fatalf("listed usage count = %+v, want 1 today / 9 total", listed)
	}
}

// setupRefreshTest 隔离账号池与重试参数，返回恢复函数。
func setupRefreshTest(t *testing.T, acc *Account) func() {
	oldClient := kit.HTTPClient
	oldPool := pool
	oldPoolPath := poolPath
	oldDelay := tokenRefreshDelay
	oldAttempts := tokenRefreshAttempts
	t.Cleanup(func() {
		kit.HTTPClient = oldClient
		pool = oldPool
		poolPath = oldPoolPath
		tokenRefreshDelay = oldDelay
		tokenRefreshAttempts = oldAttempts
	})

	poolPath = t.TempDir() + "/accounts.json"
	pool = &AccountPool{Accounts: []*Account{acc}}
	tokenRefreshDelay = 0
	return func() { kit.HTTPClient = oldClient }
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestRefreshAccountTokenKeepsStatusOnTransientFailure(t *testing.T) {
	acc := &Account{AccountID: "transient", Email: "a@example.com", RefreshToken: "rt", Status: "active"}
	setupRefreshTest(t, acc)

	requests := 0
	kit.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return jsonResponse(http.StatusInternalServerError, `{"error":"internal"}`), nil
	})}

	if err := refreshAccountToken(acc); err == nil {
		t.Fatal("expected transient refresh failure")
	}
	if acc.Status != "active" {
		t.Fatalf("status = %q, want active (transient failure must not expire account)", acc.Status)
	}
	if want := tokenRefreshAttempts + 1; requests != want {
		t.Fatalf("requests = %d, want %d (1 initial + %d retries)", requests, want, tokenRefreshAttempts)
	}
}

func TestRefreshAccountTokenExpiresOnInvalidGrant(t *testing.T) {
	acc := &Account{AccountID: "invalid", Email: "b@example.com", RefreshToken: "rt", Status: "active"}
	setupRefreshTest(t, acc)

	requests := 0
	kit.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return jsonResponse(http.StatusBadRequest, `{"error":"failed to refresh token: invalid_grant"}`), nil
	})}

	if err := refreshAccountToken(acc); err == nil {
		t.Fatal("expected refresh failure")
	}
	if acc.Status != "expired" {
		t.Fatalf("status = %q, want expired", acc.Status)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1 (invalid_grant must not be retried)", requests)
	}
}

func TestRefreshAccountTokenPreservesCooldownOnSuccess(t *testing.T) {
	cooldownUntil := time.Now().Add(2 * time.Hour)
	acc := &Account{
		AccountID:     "cooldown",
		Email:         "c@example.com",
		RefreshToken:  "rt",
		Status:        "cooldown",
		CooldownUntil: cooldownUntil,
		LastReason:    "429: daily limit",
	}
	setupRefreshTest(t, acc)
	kit.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"data":{"accessToken":"new-access","refreshToken":"new-rt","expiresAt":`+itoa(time.Now().Add(time.Hour).UnixMilli())+`}}`), nil
	})}

	if err := refreshAccountToken(acc); err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if acc.Status != "cooldown" {
		t.Fatalf("status = %q, want cooldown (token refresh must not clear rate-limit cooldown)", acc.Status)
	}
	if !acc.CooldownUntil.Equal(cooldownUntil) {
		t.Fatalf("cooldownUntil = %v, want %v", acc.CooldownUntil, cooldownUntil)
	}
	if acc.AccessToken != "workos:new-access" || acc.RefreshToken != "new-rt" {
		t.Fatalf("tokens = %q / %q, want rotated tokens", acc.AccessToken, acc.RefreshToken)
	}
}

func TestRecoverExpiredTokensActivatesAfterUpstreamRecovers(t *testing.T) {
	acc := &Account{AccountID: "recover", Email: "d@example.com", RefreshToken: "rt", Status: "expired"}
	setupRefreshTest(t, acc)
	kit.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"data":{"accessToken":"ok","expiresAt":`+itoa(time.Now().Add(time.Hour).UnixMilli())+`}}`), nil
	})}

	recoverExpiredTokens()

	if acc.Status != "active" {
		t.Fatalf("status = %q, want active after recovery", acc.Status)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
