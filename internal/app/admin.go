package app

import (
	"bufio"
	"bytes"
	"cline-go-proxy/internal/cline"
	"cline-go-proxy/internal/kit"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

var adminPassword string

func SetAdminPassword(password string) {
	adminPassword = strings.TrimSpace(password)
}

// In-memory OAuth login state for async browser login
var (
	oauthSessions   = make(map[string]*oauthSessionState)
	oauthSessionsMu sync.Mutex
)

type oauthSessionState struct {
	DeviceCode string
	UserCode   string
	AuthURL    string
	CreatedAt  time.Time
	Done       bool
	Success    bool
	Email      string
	Error      string
}

type apiResponse struct {
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

func writeAPI(w http.ResponseWriter, status int, resp apiResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

func registerAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/admin/", adminAuthHandler(adminStaticHandler))
	mux.HandleFunc("/admin/api/accounts", adminAuthHandler(corsHandler(handleAdminAccounts)))
	mux.HandleFunc("/admin/api/accounts/add", adminAuthHandler(corsHandler(handleAdminAccountAdd)))
	mux.HandleFunc("/admin/api/accounts/delete", adminAuthHandler(corsHandler(handleAdminAccountDelete)))
	mux.HandleFunc("/admin/api/accounts/test", adminAuthHandler(corsHandler(handleAdminAccountTest)))
	mux.HandleFunc("/admin/api/oauth/start", adminAuthHandler(corsHandler(handleOAuthStart)))
	mux.HandleFunc("/admin/api/oauth/status", adminAuthHandler(corsHandler(handleOAuthStatus)))
	mux.HandleFunc("/admin/api/sso/import", adminAuthHandler(corsHandler(handleSSOImport)))
	mux.HandleFunc("/admin/api/stats", adminAuthHandler(corsHandler(handleAdminStats)))
	mux.HandleFunc("/admin/api/batch-import", adminAuthHandler(corsHandler(handleBatchImport)))
	mux.HandleFunc("/admin/api/accounts/refresh-all", adminAuthHandler(corsHandler(handleAdminRefreshAll)))
	mux.HandleFunc("/admin/api/accounts/delete-all", adminAuthHandler(corsHandler(handleAdminDeleteAll)))
	mux.HandleFunc("/admin/api/accounts/reset", adminAuthHandler(corsHandler(handleAdminAccountReset)))
	mux.HandleFunc("/admin/api/accounts/export", adminAuthHandler(corsHandler(handleAccountsExport)))
	mux.HandleFunc("/admin/api/logs", adminAuthHandler(corsHandler(handleRequestLogs)))
	mux.HandleFunc("/admin/api/keys", adminAuthHandler(corsHandler(handleAdminGetKeys)))
	mux.HandleFunc("/admin/api/keys/generate", adminAuthHandler(corsHandler(handleAdminGenerateKey)))
	mux.HandleFunc("/admin/api/keys/delete", adminAuthHandler(corsHandler(handleAdminDeleteKey)))
	mux.HandleFunc("/admin/api/models", adminAuthHandler(corsHandler(handleAdminModels)))
	mux.HandleFunc("/admin/api/models/refresh", adminAuthHandler(corsHandler(handleAdminModelsRefresh)))
	mux.HandleFunc("/admin/api/config", adminAuthHandler(corsHandler(handleAdminConfig)))
	mux.HandleFunc("/admin/api/config/update", adminAuthHandler(corsHandler(handleAdminUpdateConfig)))
	mux.HandleFunc("/admin/api/opencode/config", adminAuthHandler(corsHandler(handleZenConfig)))
	mux.HandleFunc("/admin/api/opencode/config/update", adminAuthHandler(corsHandler(handleZenConfigUpdate)))
	mux.HandleFunc("/admin/api/opencode/models", adminAuthHandler(corsHandler(handleZenModels)))
	mux.HandleFunc("/admin/api/opencode/models/refresh", adminAuthHandler(corsHandler(handleZenModelsRefresh)))
	mux.HandleFunc("/admin/api/opencode/stats", adminAuthHandler(corsHandler(handleZenStats)))
	// 旧 zen 路径别名,兼容旧引用
	mux.HandleFunc("/admin/api/zen/config", adminAuthHandler(corsHandler(handleZenConfig)))
	mux.HandleFunc("/admin/api/zen/config/update", adminAuthHandler(corsHandler(handleZenConfigUpdate)))
	mux.HandleFunc("/admin/api/zen/models", adminAuthHandler(corsHandler(handleZenModels)))
	mux.HandleFunc("/admin/api/zen/models/refresh", adminAuthHandler(corsHandler(handleZenModelsRefresh)))
	mux.HandleFunc("/admin/api/zen/stats", adminAuthHandler(corsHandler(handleZenStats)))
	mux.HandleFunc("/admin/zen/", adminAuthHandler(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	}))
}

func adminAuthHandler(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if adminPassword == "" {
			writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "admin authentication is disabled; set ADMIN_PASSWORD"})
			return
		}

		const user = "admin"
		_, password, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(user), []byte("admin")) != 1 ||
			subtle.ConstantTimeCompare([]byte(password), []byte(adminPassword)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Cline Proxy Admin", charset="UTF-8"`)
			writeAPI(w, http.StatusUnauthorized, apiResponse{Error: "unauthorized"})
			return
		}

		next(w, r)
	}
}

func adminStaticHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin/" || r.URL.Path == "/admin" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(adminHTML))
		return
	}
	http.NotFound(w, r)
}

// GET /admin/api/accounts
func handleAdminAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	accounts := ListAccounts()
	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"accounts":  accounts,
			"total":     len(accounts),
			"poolIndex": loadPool().CurrentIdx,
		},
	})
}

// POST /admin/api/accounts/add  body: { refreshToken, email }
func handleAdminAccountAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		RefreshToken string `json:"refreshToken"`
		Email        string `json:"email"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.RefreshToken == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "refreshToken is required"})
		return
	}

	// Validate by refreshing
	resp, err := cline.RefreshClineToken(req.RefreshToken)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid refreshToken: " + err.Error()})
		return
	}

	if req.Email == "" {
		req.Email = fmt.Sprintf("user_%d", len(loadPool().Accounts)+1)
	}

	acc := &Account{
		AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
		Email:        req.Email,
		RefreshToken: req.RefreshToken,
		AccessToken:  "workos:" + resp.Data.AccessToken,
		ExpiresAt:    cline.ParseExpiry(resp.Data.ExpiresAt) - 60000,
		Status:       "active",
		CreatedAt:    time.Now(),
	}
	if resp.Data.RefreshToken != "" {
		acc.RefreshToken = resp.Data.RefreshToken
	}

	addAccount(acc)
	log.Printf("Account added via API: %s", req.Email)

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: fmt.Sprintf("Account %s added", req.Email),
		Data: map[string]any{
			"accountId": acc.AccountID,
			"email":     acc.Email,
			"status":    acc.Status,
		},
	})
}

// POST /admin/api/accounts/delete  body: { accountId }
func handleAdminAccountDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.AccountID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "accountId is required"})
		return
	}

	if removeAccount(req.AccountID) {
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Account deleted"})
	} else {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "Account not found"})
	}
}

// POST /admin/api/oauth/start  -- Start OAuth device login, returns URL
func handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	device, err := cline.WorkosDeviceAuth()
	if err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: err.Error()})
		return
	}

	authURL := device.VerificationURIComplete
	if authURL == "" {
		authURL = device.VerificationURI
	}

	sessionID := fmt.Sprintf("oauth_%d", time.Now().UnixMilli())
	state := &oauthSessionState{
		DeviceCode: device.DeviceCode,
		UserCode:   device.UserCode,
		AuthURL:    authURL,
		CreatedAt:  time.Now(),
	}

	oauthSessionsMu.Lock()
	oauthSessions[sessionID] = state
	oauthSessionsMu.Unlock()

	// Start polling in background
	go func() {
		interval := device.Interval
		if interval < 5 {
			interval = 5
		}
		expiresIn := device.ExpiresIn
		if expiresIn <= 0 {
			expiresIn = 300
		}

		workosTok, err := cline.PollWorkosToken(device.DeviceCode, interval, expiresIn)
		if err != nil {
			oauthSessionsMu.Lock()
			state.Error = err.Error()
			state.Done = true
			state.Success = false
			oauthSessionsMu.Unlock()
			return
		}

		reg, err := cline.RegisterWithCline(workosTok.AccessToken, workosTok.RefreshToken)
		if err != nil {
			oauthSessionsMu.Lock()
			state.Error = err.Error()
			state.Done = true
			state.Success = false
			oauthSessionsMu.Unlock()
			return
		}

		email := "unknown"
		if reg.Data.UserInfo != nil && reg.Data.UserInfo.Email != "" {
			email = reg.Data.UserInfo.Email
		}

		acc := &Account{
			AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
			Email:        email,
			RefreshToken: reg.Data.RefreshToken,
			AccessToken:  "workos:" + reg.Data.AccessToken,
			ExpiresAt:    cline.ParseExpiry(reg.Data.ExpiresAt) - 60000,
			Status:       "active",
			CreatedAt:    time.Now(),
		}
		addAccount(acc)

		oauthSessionsMu.Lock()
		state.Done = true
		state.Success = true
		state.Email = email
		oauthSessionsMu.Unlock()
		log.Printf("OAuth account added: %s", email)
	}()

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"sessionId":       sessionID,
			"verificationUri": authURL,
			"userCode":        device.UserCode,
		},
	})
}

// GET /admin/api/oauth/status?sessionId=xxx
func handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "sessionId required"})
		return
	}

	oauthSessionsMu.Lock()
	state, ok := oauthSessions[sessionID]
	oauthSessionsMu.Unlock()

	if !ok {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "session not found"})
		return
	}

	resp := map[string]any{
		"done":    state.Done,
		"success": state.Success,
	}
	if state.Done {
		resp["email"] = state.Email
		if !state.Success {
			resp["error"] = state.Error
		}
	}

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: resp})
}

// POST /admin/api/sso/import  body: { ssoCookies: string, email?: string }
func handleSSOImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		SSOCookies string `json:"ssoCookies"`
		Email      string `json:"email"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.SSOCookies == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "ssoCookies is required"})
		return
	}

	// SSO cookies import - try to use WorkOS device auth (requires browser)
	// For direct SSO cookie conversion, we'd need the WorkOS session cookie
	// to exchange for tokens. This is a placeholder that accepts WorkOS session
	// cookies. In practice, users should use OAuth or direct refreshToken.
	//
	// SSO cookie format expected: workos_session=xxx or similar
	lines := strings.Split(req.SSOCookies, "\n")
	imported := 0
	errors := []string{}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Try to use the cookie as a refresh token directly (common format)
		if strings.HasPrefix(line, "workos:") || len(line) > 20 {
			token := strings.TrimPrefix(line, "workos:")
			resp, err := cline.RefreshClineToken(token)
			if err != nil {
				errors = append(errors, fmt.Sprintf("token %s...: %v", kit.Truncate(token, 16), err))
				continue
			}
			email := req.Email
			if email == "" {
				email = fmt.Sprintf("sso_user_%d", time.Now().UnixMilli())
			}

			acc := &Account{
				AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
				Email:        email,
				RefreshToken: token,
				AccessToken:  "workos:" + resp.Data.AccessToken,
				ExpiresAt:    cline.ParseExpiry(resp.Data.ExpiresAt) - 60000,
				Status:       "active",
				CreatedAt:    time.Now(),
			}
			addAccount(acc)
			imported++
		}
	}

	result := map[string]any{
		"imported": imported,
		"failed":   len(errors),
	}
	if len(errors) > 0 {
		result["errors"] = errors
	}

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: fmt.Sprintf("Imported %d accounts, %d failed", imported, len(errors)),
		Data:    result,
	})
}

// POST /admin/api/batch-import  body: { tokens: [{ refreshToken, email }] }
func handleBatchImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		Tokens []struct {
			RefreshToken string `json:"refreshToken"`
			Email        string `json:"email"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if len(req.Tokens) == 0 {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "tokens array is empty"})
		return
	}

	imported := 0
	errors := []string{}

	for _, t := range req.Tokens {
		if t.RefreshToken == "" {
			continue
		}
		resp, err := cline.RefreshClineToken(t.RefreshToken)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", t.Email, err))
			continue
		}
		email := t.Email
		if email == "" {
			email = fmt.Sprintf("batch_%d", time.Now().UnixMilli())
		}
		acc := &Account{
			AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
			Email:        email,
			RefreshToken: t.RefreshToken,
			AccessToken:  "workos:" + resp.Data.AccessToken,
			ExpiresAt:    cline.ParseExpiry(resp.Data.ExpiresAt) - 60000,
			Status:       "active",
			CreatedAt:    time.Now(),
		}
		addAccount(acc)
		imported++
	}

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: fmt.Sprintf("Imported %d accounts, %d failed", imported, len(errors)),
		Data: map[string]any{
			"imported": imported,
			"failed":   len(errors),
			"errors":   errors,
		},
	})
}

// POST /admin/api/accounts/refresh-all
func handleAdminRefreshAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	p := loadPool()
	poolMu.Lock()
	accounts := append([]*Account(nil), p.Accounts...)
	poolMu.Unlock()
	for _, a := range accounts {
		if err := refreshAccountToken(a); err != nil {
			log.Printf("Refresh failed for %s: %v", a.Email, err)
		}
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "All tokens refreshed"})
}

// POST /admin/api/accounts/delete-all
func handleAdminDeleteAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	poolMu.Lock()
	pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}}
	poolMu.Unlock()
	savePool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "All accounts deleted"})
}

// POST /admin/api/accounts/reset  body: { accountId }
// 检测限流并解除：向上游发送探测请求。若上游仍限流（429）则保持冷却，
// 重置无效；若探测成功则清除冷却、恢复正常状态，并重置今日统计。
func handleAdminAccountReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	acc := getAccountByID(req.AccountID)
	if acc == nil {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "account not found"})
		return
	}

	result, status := testAccount(acc)

	if status == "active" {
		// 探测通过：解除冷却并重置今日统计
		resetTodayUsage(acc)
		writeAPI(w, http.StatusOK, apiResponse{
			Success: true,
			Message: "检测通过：上游未限流，已解除冷却并重置今日统计",
			Data:    result,
		})
		return
	}

	// 仍限流/失效：保持冷却，重置无效
	msg := "上游仍限流，重置无效，保持冷却"
	if status == "expired" {
		msg = "Token 已失效，重置无效"
	} else if status == "error" {
		msg = "探测异常，请稍后重试"
	}
	if until, ok := result["cooldownUntil"].(string); ok && until != "" {
		msg += "（预计恢复 " + until + "）"
	}
	if remaining, ok := result["remaining"].(string); ok && remaining != "" {
		msg += "（剩余 " + remaining + "）"
	}
	writeAPI(w, http.StatusOK, apiResponse{
		Success: false,
		Message: msg,
		Data:    result,
	})
}

// POST /admin/api/accounts/test  body: { accountId }
// 用指定账号发送一个轻量探测请求，验证该账号是否能返回有效助手回复。
// 如果命中 429/INFERENCE_CAP_ERROR，自动标记冷却并返回预计恢复时间。
func handleAdminAccountTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	if req.AccountID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "accountId is required"})
		return
	}

	acc := getAccountByID(req.AccountID)
	if acc == nil {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "account not found"})
		return
	}

	result, status := testAccount(acc)
	reason, _ := result["reason"].(string)
	log.Printf("Test account %s: status=%s reason=%s", truncateEmail(acc.Email), status, reason)

	writeAPI(w, http.StatusOK, apiResponse{
		Success: status == "active",
		Message: status,
		Data:    result,
	})
}

// testAccount 对单个账号执行轻量探测请求，返回详细结果与最终状态。
// 测试按钮是"升级版重置"：无论账号当前是 active/cooldown/expired，
// 都会尝试刷新 Token 并发起一次真实探测；只有收到有效助手回复才清除异常状态。
// 返回的 status: active / cooldown / expired / error
func testAccount(acc *Account) (map[string]any, string) {
	prevStatus := acc.Status
	prevCooldownUntil := acc.CooldownUntil
	prevLastReason := acc.LastReason
	restorePreviousState := func() {
		poolMu.Lock()
		acc.Status = prevStatus
		acc.CooldownUntil = prevCooldownUntil
		acc.LastReason = prevLastReason
		savePoolLocked()
		poolMu.Unlock()
	}

	// 取 token（expired/cooldown 也尝试刷新，测试按钮不因状态直接拒绝）
	token, err := ensureAccountToken(acc)
	if err != nil {
		poolMu.Lock()
		acc.LastReason = "token refresh failed: " + err.Error()
		acc.Status = "expired"
		acc.CooldownUntil = time.Time{}
		savePoolLocked()
		poolMu.Unlock()
		return map[string]any{
			"accountId":  acc.AccountID,
			"email":      acc.Email,
			"status":     "expired",
			"reason":     acc.LastReason,
			"prevStatus": prevStatus,
		}, "expired"
	}

	// 构造轻量探测请求：单条用户消息和较小的输出上限。探测请求需与正常代理
	// 请求使用相同的模型选择、流式策略和任务 ID，否则部分模型会返回空响应。
	probeModel := getDefaultModel()
	sessionID := fmt.Sprintf("test_%d", time.Now().UnixMilli())
	probeBody := map[string]any{
		"model": probeModel,
		// max_tokens=1 在 GLM 的思考模式下可能只产生 reasoning，导致没有最终回复。
		"max_tokens":       32,
		"session_id":       sessionID,
		"reasoning_effort": defaultReasoningEffort,
		"messages": []map[string]any{
			{"role": "user", "content": "Reply with exactly: pong"},
		},
	}
	if modelNeedsStream(probeModel) {
		probeBody["stream"] = true
	}
	bodyJSON, _ := json.Marshal(probeBody)

	req, err := http.NewRequest("POST", cline.ClineAPIBase+"/chat/completions", bytes.NewReader(bodyJSON))
	if err != nil {
		restorePreviousState()
		return map[string]any{
			"accountId": acc.AccountID,
			"email":     acc.Email,
			"status":    "error",
			"reason":    "build request: " + err.Error(),
		}, "error"
	}
	req.Header = clineHeaders(token, sessionID)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := kit.HTTPClient.Do(req)
	if err != nil {
		// 网络错误：5 分钟短冷却
		markAccountCooldown(acc, "network error: "+err.Error(), 5*time.Minute)
		return map[string]any{
			"accountId":     acc.AccountID,
			"email":         acc.Email,
			"status":        "cooldown",
			"reason":        acc.LastReason,
			"cooldownUntil": acc.CooldownUntil.Format("2006-01-02 15:04:05"),
			"remaining":     formatDuration(time.Until(acc.CooldownUntil)),
		}, "cooldown"
	}
	defer resp.Body.Close()

	bodyBytes, readErr := io.ReadAll(resp.Body)
	bodyStr := string(bodyBytes)
	if readErr != nil {
		markAccountCooldown(acc, "probe response read failed: "+readErr.Error(), 5*time.Minute)
		return map[string]any{
			"accountId":     acc.AccountID,
			"email":         acc.Email,
			"status":        "cooldown",
			"reason":        acc.LastReason,
			"cooldownUntil": acc.CooldownUntil.Format("2006-01-02 15:04:05"),
			"remaining":     formatDuration(time.Until(acc.CooldownUntil)),
			"httpStatus":    resp.StatusCode,
			"probeValid":    false,
		}, "cooldown"
	}

	if resp.StatusCode == 429 {
		duration := parseInferenceCapDuration(bodyStr)
		if duration <= 0 {
			duration = parseRetryAfter(resp.Header.Get("Retry-After"))
		}
		reason := kit.Truncate(bodyStr, 500)
		markAccountCooldown(acc, "429: "+reason, duration)
		log.Printf("Test hit 429 on %s, cooldown %v", truncateEmail(acc.Email), duration)
		return map[string]any{
			"accountId":     acc.AccountID,
			"email":         acc.Email,
			"status":        "cooldown",
			"reason":        acc.LastReason,
			"cooldownUntil": acc.CooldownUntil.Format("2006-01-02 15:04:05"),
			"remaining":     formatDuration(time.Until(acc.CooldownUntil)),
			"httpStatus":    resp.StatusCode,
			"probeValid":    false,
		}, "cooldown"
	}

	if resp.StatusCode == 401 {
		poolMu.Lock()
		acc.Status = "expired"
		acc.LastReason = "401 unauthorized"
		acc.CooldownUntil = time.Time{}
		savePoolLocked()
		poolMu.Unlock()
		return map[string]any{
			"accountId":  acc.AccountID,
			"email":      acc.Email,
			"status":     "expired",
			"reason":     acc.LastReason,
			"httpStatus": resp.StatusCode,
			"probeValid": false,
		}, "expired"
	}

	if resp.StatusCode != 200 {
		// 其它错误：不强制冷却，按一次失败处理
		restorePreviousState()
		return map[string]any{
			"accountId":  acc.AccountID,
			"email":      acc.Email,
			"status":     "error",
			"reason":     fmt.Sprintf("API %d: %s", resp.StatusCode, kit.Truncate(bodyStr, 300)),
			"httpStatus": resp.StatusCode,
			"probeValid": false,
		}, "error"
	}

	// 上游可能在 HTTP 200 下返回空响应、错误事件或只有 reasoning 的 SSE。
	// 仅凭状态码不能证明账号真的能回复，必须验证响应体中的有效 assistant 输出。
	validation := validateProbeResponse(bodyBytes)
	if !validation.valid {
		reason := validation.reason
		if reason == "" {
			reason = "no valid assistant reply"
		}
		if validation.rateLimited {
			duration := parseInferenceCapDuration(bodyStr)
			if duration <= 0 {
				duration = parseRetryAfter(resp.Header.Get("Retry-After"))
			}
			markAccountCooldown(acc, "200 probe rate limit: "+kit.Truncate(bodyStr, 500), duration)
			log.Printf("Test hit embedded rate limit on %s, cooldown %v", truncateEmail(acc.Email), duration)
			return map[string]any{
				"accountId":     acc.AccountID,
				"email":         acc.Email,
				"status":        "cooldown",
				"reason":        acc.LastReason,
				"cooldownUntil": acc.CooldownUntil.Format("2006-01-02 15:04:05"),
				"remaining":     formatDuration(time.Until(acc.CooldownUntil)),
				"httpStatus":    resp.StatusCode,
				"probeValid":    false,
			}, "cooldown"
		}

		restorePreviousState()
		return map[string]any{
			"accountId":  acc.AccountID,
			"email":      acc.Email,
			"status":     "error",
			"reason":     reason,
			"httpStatus": resp.StatusCode,
			"probeValid": false,
			"prevStatus": prevStatus,
		}, "error"
	}

	// 成功：确认收到有效 assistant 输出后，才清除所有异常状态（冷却/过期/原因）。
	poolMu.Lock()
	acc.Status = "active"
	acc.LastReason = ""
	acc.CooldownUntil = time.Time{}
	poolMu.Unlock()
	bumpUsage(acc)
	return map[string]any{
		"accountId":  acc.AccountID,
		"email":      acc.Email,
		"status":     "active",
		"reason":     "ok: assistant reply received",
		"httpStatus": resp.StatusCode,
		"prevStatus": prevStatus,
		"probeValid": true,
	}, "active"
}

type probeValidation struct {
	valid       bool
	rateLimited bool
	reason      string
}

// validateProbeResponse 验证探测响应中是否存在有效的 assistant 文本或 tool call。
// Cline 上游通常返回 chat-completions JSON 或 SSE；两种格式都可能在 HTTP 200
// 下携带错误，所以不能只看响应状态码。
func validateProbeResponse(body []byte) probeValidation {
	raw := strings.TrimSpace(string(body))
	if raw == "" {
		return probeValidation{reason: "empty upstream response"}
	}
	if isProbeRateLimitText(raw) {
		return probeValidation{rateLimited: true, reason: "upstream rate-limit response"}
	}

	var obj map[string]any
	if json.Unmarshal(body, &obj) == nil {
		return validateProbeObject(obj)
	}

	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 2*1024*1024)
	sawData := false
	var firstFailure probeValidation
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		sawData = true

		var event map[string]any
		if json.Unmarshal([]byte(payload), &event) != nil {
			continue
		}
		result := validateProbeObject(event)
		if result.rateLimited {
			return result
		}
		if result.valid {
			return result
		}
		if firstFailure.reason == "" {
			firstFailure = result
		}
	}
	if err := scanner.Err(); err != nil {
		return probeValidation{reason: "read SSE response: " + err.Error()}
	}
	if firstFailure.reason != "" {
		return firstFailure
	}
	if sawData {
		return probeValidation{reason: "SSE contained no assistant reply"}
	}
	return probeValidation{reason: "invalid upstream response"}
}

func validateProbeObject(obj map[string]any) probeValidation {
	if reason, ok := probeObjectError(obj); ok {
		return probeValidation{
			rateLimited: isProbeRateLimitText(reason),
			reason:      "upstream error: " + reason,
		}
	}

	obj = unwrapProbeObject(obj)
	if reason, ok := probeObjectError(obj); ok {
		return probeValidation{
			rateLimited: isProbeRateLimitText(reason),
			reason:      "upstream error: " + reason,
		}
	}
	if probeObjectHasAssistantOutput(obj) {
		return probeValidation{valid: true}
	}
	return probeValidation{reason: "response contained no assistant text or tool call"}
}

func unwrapProbeObject(obj map[string]any) map[string]any {
	for i := 0; i < 3; i++ {
		if _, ok := obj["choices"]; ok {
			return obj
		}
		switch data := obj["data"].(type) {
		case map[string]any:
			obj = data
		case []any:
			if len(data) == 0 {
				return obj
			}
			if nested, ok := data[0].(map[string]any); ok {
				obj = nested
				continue
			}
			return obj
		default:
			return obj
		}
	}
	return obj
}

func probeObjectError(obj map[string]any) (string, bool) {
	if value, ok := obj["error"]; ok && value != nil {
		return probeValueText(value), true
	}
	if status, ok := obj["status"].(string); ok && strings.EqualFold(status, "error") {
		if message, ok := obj["message"].(string); ok && strings.TrimSpace(message) != "" {
			return message, true
		}
		return "status=error", true
	}
	for _, key := range []string{"code", "type"} {
		value, ok := obj[key].(string)
		if ok && (strings.Contains(strings.ToLower(value), "error") || isProbeRateLimitText(value)) {
			if message, ok := obj["message"].(string); ok && strings.TrimSpace(message) != "" {
				return message, true
			}
			return value, true
		}
	}
	return "", false
}

func probeValueText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if obj, ok := value.(map[string]any); ok {
		for _, key := range []string{"message", "code", "type"} {
			if text, ok := obj[key].(string); ok && strings.TrimSpace(text) != "" {
				return text
			}
		}
	}
	encoded, err := json.Marshal(value)
	if err == nil {
		return string(encoded)
	}
	return fmt.Sprint(value)
}

func probeObjectHasAssistantOutput(obj map[string]any) bool {
	choices, ok := obj["choices"].([]any)
	if !ok {
		return false
	}
	for _, rawChoice := range choices {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"message", "delta"} {
			part, ok := choice[key].(map[string]any)
			if !ok {
				continue
			}
			if probeHasText(part["content"]) || probeHasToolCall(part) {
				return true
			}
		}
	}
	return false
}

func probeHasText(value any) bool {
	switch text := value.(type) {
	case string:
		return strings.TrimSpace(text) != ""
	case []any:
		for _, item := range text {
			if probeHasText(item) {
				return true
			}
			if obj, ok := item.(map[string]any); ok {
				if probeHasText(obj["text"]) || probeHasText(obj["content"]) {
					return true
				}
			}
		}
	}
	return false
}

func probeHasToolCall(part map[string]any) bool {
	if calls, ok := part["tool_calls"].([]any); ok && len(calls) > 0 {
		return true
	}
	if call, ok := part["function_call"].(map[string]any); ok && len(call) > 0 {
		return true
	}
	return false
}

func isProbeRateLimitText(text string) bool {
	upper := strings.ToUpper(text)
	for _, marker := range []string{
		"INFERENCE_CAP_ERROR",
		"TRY AGAIN IN",
		"TOO MANY REQUESTS",
		"RATE_LIMIT",
		"RATE LIMIT",
		"DAILY FREE LIMIT",
		"QUOTA_EXCEEDED",
	} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	days := int(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	hours := int(d / time.Hour)
	d -= time.Duration(hours) * time.Hour
	mins := int(d / time.Minute)
	d -= time.Duration(mins) * time.Minute
	secs := int(d / time.Second)
	parts := []string{}
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if mins > 0 {
		parts = append(parts, fmt.Sprintf("%dm", mins))
	}
	if secs > 0 && days == 0 && hours == 0 {
		parts = append(parts, fmt.Sprintf("%ds", secs))
	}
	if len(parts) == 0 {
		return "0s"
	}
	return strings.Join(parts, " ")
}

// Global proxy config (mutable via API)
var (
	proxyConfig   = defaultProxyConfig()
	proxyConfigMu sync.Mutex
)

type proxyConfigData struct {
	Strategy string            `json:"strategy"`
	Headers  map[string]string `json:"headers"`
}

func defaultProxyConfig() *proxyConfigData {
	return &proxyConfigData{
		Strategy: "round_robin",
		Headers: map[string]string{
			"User-Agent":         "Cline/3.0.50",
			"HTTP-Referer":       "https://cline.bot",
			"X-Title":            "Cline",
			"X-IS-MULTIROOT":     "false",
			"X-CLIENT-TYPE":      "cline-cli",
			"X-CLIENT-VERSION":   "3.0.50",
			"X-PLATFORM":         "terminal",
			"X-PLATFORM-VERSION": "3.0.50",
			"X-CORE-VERSION":     "0.0.70",
		},
	}
}

func getProxyConfig() *proxyConfigData {
	proxyConfigMu.Lock()
	defer proxyConfigMu.Unlock()
	return proxyConfig
}

func setProxyConfig(c *proxyConfigData) {
	proxyConfigMu.Lock()
	defer proxyConfigMu.Unlock()
	proxyConfig = c
}

// GET /admin/api/keys
func handleAdminGetKeys(w http.ResponseWriter, r *http.Request) {
	p := loadPool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"keys": p.Keys}})
}

// POST /admin/api/keys/generate
func handleAdminGenerateKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	key := fmt.Sprintf("cline_%x_%x", time.Now().UnixMilli(), time.Now().UnixNano()%1000000)
	p := loadPool()
	poolMu.Lock()
	p.Keys = append(p.Keys, key)
	poolMu.Unlock()
	savePool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"key": key}})
}

// POST /admin/api/keys/delete  body: { key }
func handleAdminDeleteKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()
	var req struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	p := loadPool()
	poolMu.Lock()
	for i, k := range p.Keys {
		if k == req.Key {
			p.Keys = append(p.Keys[:i], p.Keys[i+1:]...)
			break
		}
	}
	poolMu.Unlock()
	savePool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Key deleted"})
}

// GET /admin/api/config
func handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	cfg := getProxyConfig()
	address := r.Host
	if address == "" {
		address = proxyListenAddress
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"address":      address,
		"strategy":     cfg.Strategy,
		"version":      "go-1.1",
		"poolPath":     poolPath,
		"defaultModel": getDefaultModel(),
		"headers":      cfg.Headers,
	}})
}

// POST /admin/api/config  body: { strategy?, headers?, defaultModel? }
func handleAdminUpdateConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		Strategy     string            `json:"strategy"`
		Headers      map[string]string `json:"headers"`
		DefaultModel string            `json:"defaultModel"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	cfg := getProxyConfig()
	changed := false

	if req.Strategy != "" {
		switch req.Strategy {
		case "round_robin", "fill", "random":
			cfg.Strategy = req.Strategy
			changed = true
		default:
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid strategy, must be: round_robin, fill, random"})
			return
		}
	}

	if req.Headers != nil {
		for k, v := range req.Headers {
			cfg.Headers[k] = v
		}
		changed = true
	}

	if req.DefaultModel != "" {
		initModelsCache()
		modelsMu.Lock()
		_, ok := modelsCache[req.DefaultModel]
		modelsMu.Unlock()
		if !ok {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "unknown model: " + req.DefaultModel})
			return
		}
		setDefaultModel(req.DefaultModel)
		changed = true
	}

	if changed {
		setProxyConfig(cfg)
	}

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"strategy":     cfg.Strategy,
		"headers":      cfg.Headers,
		"defaultModel": defaultModel,
	}})
}

// GET /admin/api/models
func handleAdminModels(w http.ResponseWriter, r *http.Request) {
	ensureModelsFresh()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"models":   getFreeModels(),
		"lastSync": modelsLastSync,
	}})
}

// POST /admin/api/models/refresh
func handleAdminModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	initModelsCache()
	modelsMu.Lock()
	syncing := modelsSyncing
	modelsMu.Unlock()
	if syncing {
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "sync already running"})
		return
	}
	go syncModelsOnce()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "model sync started"})
}

// GET /admin/api/stats
func handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	p := loadPool()
	active, cooldown, expired := 0, 0, 0
	for _, a := range p.Accounts {
		switch a.Status {
		case "active":
			active++
		case "cooldown":
			cooldown++
		case "expired":
			expired++
		}
	}

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"total":    len(p.Accounts),
			"active":   active,
			"cooldown": cooldown,
			"expired":  expired,
			"strategy": "round_robin",
			"version":  "go-1.1",
		},
	})
}

// GET /admin/api/accounts/export 导出全部账号 refreshToken（JSON 文件下载）
func handleAccountsExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	p := loadPool()
	items := make([]map[string]any, 0, len(p.Accounts))
	for _, a := range p.Accounts {
		items = append(items, map[string]any{
			"refreshToken": a.RefreshToken,
			"email":        a.Email,
		})
	}
	data, _ := json.MarshalIndent(items, "", "  ")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="cline-accounts-export.json"`)
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// GET /admin/api/logs 最近请求日志（对话/调用历史）
func handleRequestLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	logs := LoadRequestLogs()
	if logs == nil {
		logs = []RequestLog{}
	}
	// 倒序返回（最新在前）
	for i, j := 0, len(logs)-1; i < j; i, j = i+1, j-1 {
		logs[i], logs[j] = logs[j], logs[i]
	}
	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"logs": logs,
		},
	})
}
