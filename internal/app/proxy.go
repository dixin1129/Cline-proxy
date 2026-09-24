package app

import (
	"bufio"
	"bytes"
	"cline-go-proxy/internal/cline"
	"cline-go-proxy/internal/kit"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

var defaultModel = "deepseek/deepseek-v4-flash"

var proxyListenAddress = "0.0.0.0:3457"

const (
	defaultMaxTokens       = 128000
	defaultReasoningEffort = "high"
	clineMaxAttempts       = 3
	clineTransientCooldown = 30 * time.Second
)

var clineRetryDelay = 250 * time.Millisecond

var passThroughKeys = []string{
	"tools", "tool_choice", "parallel_tool_calls", "functions", "function_call",
	"temperature", "top_p", "top_k", "stop", "presence_penalty", "frequency_penalty",
	"response_format", "user", "n", "logit_bias", "seed", "logprobs", "top_logprobs",
	"stream_options", "metadata",
}

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            json.RawMessage `json:"messages"`
	Stream              bool            `json:"stream,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Tools               json.RawMessage `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	ReasoningEffortAlt  string          `json:"reasoningEffort,omitempty"`
	Extra               map[string]any  `json:"-"`
}

func StartProxy(host string, port int) error {
	if strings.TrimSpace(host) == "" {
		host = "0.0.0.0"
	}
	initLogFile()

	p := loadPool()
	activeCount := 0
	for _, a := range p.Accounts {
		if a.Status == "active" {
			// Try to pre-warm tokens
			if a.AccessToken == "" || time.Now().UnixMilli() >= a.ExpiresAt {
				if err := refreshAccountToken(a); err != nil {
					log.Printf("  Pre-warm failed for %s: %v", a.Email, err)
					continue
				}
			}
			activeCount++
		}
	}
	log.Printf("Loaded %d active accounts from pool", activeCount)

	startModelsRefresher()
	startZenModelsRefresher()
	startTokenRecoveryLoop()
	initStats()
	LoadRequestLogsFromFile()
	go cleanupCompactStates()

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})

	mux.HandleFunc("/v1/health", corsHandler(func(w http.ResponseWriter, r *http.Request) {
		info := map[string]any{
			"status":         "ok",
			"version":        "go-1.1",
			"activeAccounts": activeCount,
		}
		writeJSON(w, http.StatusOK, info)
	}))
	mux.HandleFunc("/health", corsHandler(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":         "ok",
			"version":        "go-1.1",
			"activeAccounts": activeCount,
		})
	}))

	// Admin API (frontend + REST)
	registerAdminRoutes(mux)

	apiKeyHandler := func(next http.HandlerFunc) http.HandlerFunc {
		return corsHandler(func(w http.ResponseWriter, r *http.Request) {
			// Allow requests without key if no keys configured
			p := loadPool()
			if len(p.Keys) == 0 {
				next(w, r)
				return
			}

			key := r.Header.Get("x-api-key")
			if key == "" {
				if b := r.Header.Get("Authorization"); len(b) > 7 && b[:7] == "Bearer " {
					key = b[7:]
				}
			}

			valid := false
			for _, k := range p.Keys {
				if k == key {
					valid = true
					break
				}
			}

			if !valid {
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error": map[string]string{
						"message": "invalid API key. Generate one at /admin/ or set x-api-key header",
						"type":    "auth_error",
					},
				})
				return
			}
			if meta, ok := r.Context().Value(apiKeyAuthMetaKey{}).(*apiKeyAuthMeta); ok {
				meta.authed = true
			}
			next(w, r)
		})
	}

	modelsHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		ensureModelsFresh()
		data := apiModelList()
		// 合并 zen 免费模型
		cfg := getZenConfig()
		if cfg.Enabled {
			for _, zm := range zenModelList() {
				data = append(data, map[string]any{
					"id":       zm["id"],
					"object":   "model",
					"created":  time.Now().UnixMilli(),
					"owned_by": "opencode-zen",
					"source":   "zen-free",
					"status":   "active",
					"cost":     "free",
					"context":  zm["context"],
					"output":   zm["output"],
				})
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
	})
	mux.HandleFunc("/v1/models", modelsHandler)
	mux.HandleFunc("/models", modelsHandler)

	chatHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if activeCount == 0 && len(loadPool().Accounts) == 0 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]string{
					"message": "No accounts in pool. Run with --add-account or POST /admin/login to add accounts.",
					"type":    "auth_error",
				},
			})
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "parse_error"},
			})
			return
		}

		var params map[string]any
		if err := json.Unmarshal(body, &params); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "parse_error"},
			})
			return
		}

		isStream, _ := params["stream"].(bool)
		toolCount := 0
		if tools, ok := params["tools"]; ok {
			if t, ok := tools.([]any); ok {
				toolCount = len(t)
			}
		}
		model, _ := params["model"].(string)
		log.Printf("  client: stream=%v tools=%d model=%s", isStream, toolCount, model)

		// Override system prompt from override.md for OpenAI format
		applyOverride(params)

		// zen 免费模型路由
		if route := routeModel(model); route == "zen" {
			handleZenChat(w, r, params)
			return
		} else if route == "reject" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": fmt.Sprintf("model %q is a paid zen model; only free zen models are proxied", model), "type": "invalid_request_error"},
			})
			return
		}

		upstreamStream := isStream
		if !isStream {
			model := getDefaultModel()
			if m, ok := params["model"].(string); ok && m != "" {
				model = normalizeRequestModel(m)
			}
			if modelNeedsStream(model) {
				upstreamStream = true
				log.Printf("  model %s requires stream: forcing upstream stream, will aggregate", model)
			}
		}

		resp, acc, err := callClineAPIContext(r.Context(), params, upstreamStream)
		if err != nil {
			log.Printf("  api error: %v", err)
			writeClineAPIError(w, err)
			return
		}
		defer resp.Body.Close()

		usageFn := accountUsageFn(acc, params)
		usageFnWithLog := func(u map[string]any) {
			setRequestLogMetaFromUsage(r, acc, u, params)
			usageFn(u)
		}

		if isStream {
			handleStreamResponseWithUsageContext(r.Context(), w, resp, usageFnWithLog)
			return
		}

		if upstreamStream {
			out, err := collectStreamResponseContext(r.Context(), resp)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return
				}
				writeJSON(w, http.StatusBadGateway, map[string]any{
					"error": map[string]string{"message": err.Error(), "type": "api_error"},
				})
				return
			}
			if u, ok := out["usage"].(map[string]any); ok && len(u) > 0 {
				usageFnWithLog(u)
			}
			out = normalizeOpenAIResponse(out)
			log.Printf("  nonstream (aggregated): model=%v content_len=%d finish=%v",
				out["model"], len(getNested(out, "choices", 0, "message", "content").(string)), getNested(out, "choices", 0, "finish_reason"))
			writeJSON(w, http.StatusOK, out)
			return
		}

		handleNonStreamResponseWithUsage(w, resp, usageFnWithLog)
	})
	mux.HandleFunc("/v1/chat/completions", chatHandler)
	mux.HandleFunc("/chat/completions", chatHandler)

	// Anthropic Messages API support
	anthropicHandler := apiKeyHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		handleAnthropicMessages(w, r)
	})
	mux.HandleFunc("/v1/messages", anthropicHandler)
	mux.HandleFunc("/messages", anthropicHandler)

	// OpenAI Responses API
	responsesHandler := apiKeyHandler(handleResponses)
	mux.HandleFunc("/v1/responses", responsesHandler)
	mux.HandleFunc("/responses", responsesHandler)

	addr := fmt.Sprintf("%s:%d", host, port)
	proxyListenAddress = addr
	server := &http.Server{
		Addr:    addr,
		Handler: requestLogMiddleware(mux),
	}

	fmt.Println("")
	fmt.Println(strings.Repeat("=", 58))
	fmt.Println("  Cline Go Proxy v1.0 - No CLI Required")
	fmt.Println(strings.Repeat("=", 58))
	fmt.Printf("  http://%s\n", addr)
	fmt.Printf("  http://%s/v1\n", addr)
	if adminPassword != "" {
		fmt.Println("  Admin:   admin / configured via ADMIN_PASSWORD")
	} else {
		fmt.Println("  Admin:   DISABLED (set ADMIN_PASSWORD)")
	}
	fmt.Println("  API Key: any value")
	fmt.Printf("  Model:   %s (auto-detected)\n", getDefaultModel())
	fmt.Printf("  Accounts: %d total, %d active\n", len(loadPool().Accounts), activeCount)
	fmt.Println(strings.Repeat("=", 58))

	return server.ListenAndServe()
}

// initLogFile 将日志同时输出到控制台与 cline-proxy.log（追加模式），
// 控制台窗口滚动内容有限，文件可完整保留所有日志。
func initLogFile() {
	path := kit.ResolveDataPath("cline-proxy.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("  open log file failed: %v", err)
		return
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
	log.Printf("========== proxy started, log file: %s ==========", path)
}

func corsHandler(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, anthropic-version, anthropic-beta")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// writeClineAPIError 将上游失败映射为客户端可判断的状态码，避免把
// 额度耗尽、上游网关错误和账号池不可用全部伪装成 HTTP 500。
func writeClineAPIError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	status := http.StatusBadGateway
	if apiErr := new(clineAPIError); errors.As(err, &apiErr) && apiErr.StatusCode >= 400 && apiErr.StatusCode <= 599 {
		status = apiErr.StatusCode
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"message": err.Error(),
			"type":    "api_error",
		},
	})
}

// applyOverride 用 override.md 替换系统提示词(不存在则跳过)
func applyOverride(params map[string]any) {
	override := loadOverrideContent()
	if override == "" {
		return
	}
	if msgs, ok := params["messages"].([]any); ok {
		found := false
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				if mm["role"] == "system" {
					mm["content"] = override
					found = true
					break
				}
			}
		}
		if !found {
			params["messages"] = append([]any{map[string]any{"role": "system", "content": override}}, msgs...)
		}
	}
}

// handleZenChat opencode zen 免费模型分支: 压缩 -> 上游 -> 透传,并记录统计
func handleZenChat(w http.ResponseWriter, r *http.Request, params map[string]any) {
	cfg := getZenConfig()
	if !cfg.Enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": "zen upstream disabled in /admin/ settings", "type": "api_error"},
		})
		return
	}
	model, _ := params["model"].(string)
	zm, ok := resolveZenFreeModel(model)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": fmt.Sprintf("model %q is not a free zen model", model), "type": "invalid_request_error"},
		})
		return
	}
	isStream, _ := params["stream"].(bool)
	tracker := newZenStatsTracker(zenStatsRecord{
		TS:           time.Now().UnixMilli(),
		Upstream:     "zen",
		Model:        zm.ID,
		Stream:       isStream,
		PromptTokens: estimateJSON(params),
	})

	sid := requestSessionID(params, r.Header)
	out := maybeCompact(params, zm, sid)
	tracker.rec.Compacted = out.changed
	tracker.rec.CompactionTokens = out.compactTokens
	if out.changed {
		log.Printf("  zen: %s", out.note)
	}

	resp, rateLimited, err := callZenAPI(params, isStream)
	if err != nil {
		log.Printf("  zen api error: %v", err)
		tracker.rec.RateLimited = rateLimited
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		tracker.finish(false, http.StatusBadGateway)
		return
	}
	tracker.rec.RateLimited = rateLimited
	defer resp.Body.Close()
	tracker.rec.Status = resp.StatusCode

	usageFn := func(u map[string]any) {
		if pt, ok := u["prompt_tokens"].(float64); ok {
			tracker.rec.CompletionTokens += int(pt) - tracker.rec.PromptTokens
			if tracker.rec.CompletionTokens < 0 {
				tracker.rec.CompletionTokens = 0
			}
		}
		if ct, ok := u["completion_tokens"].(float64); ok {
			tracker.rec.CompletionTokens = int(ct)
		}
	}

	if isStream {
		handleStreamResponseWithUsageContext(r.Context(), w, resp, usageFn)
		tracker.finish(true, resp.StatusCode)
		return
	}
	handleNonStreamResponseWithUsage(w, resp, usageFn)
	tracker.finish(true, resp.StatusCode)
}

func cleanMessages(messages []any) []any {
	cleaned := make([]any, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			cleaned = append(cleaned, m)
			continue
		}
		cleaned = append(cleaned, msg)
	}
	return cleaned
}

func buildUpstreamBody(params map[string]any, stream bool) map[string]any {
	sessionID := fmt.Sprintf("sess_%d", time.Now().UnixMilli())

	maxTokens := defaultMaxTokens
	if mt, ok := params["max_tokens"].(float64); ok {
		maxTokens = int(mt)
	} else if mt, ok := params["max_completion_tokens"].(float64); ok {
		maxTokens = int(mt)
	}

	model := getDefaultModel()
	if m, ok := params["model"].(string); ok && m != "" {
		model = normalizeRequestModel(m)
	}

	body := map[string]any{
		"model":            model,
		"max_tokens":       maxTokens,
		"session_id":       sessionID,
		"reasoning_effort": defaultReasoningEffort,
	}

	if msgsRaw, ok := params["messages"]; ok {
		if msgsArr, ok := msgsRaw.([]any); ok {
			body["messages"] = cleanMessages(msgsArr)
		} else {
			body["messages"] = msgsRaw
		}
	}

	if stream {
		body["stream"] = true
	}

	if re, ok := params["reasoning_effort"].(string); ok && re != "" {
		body["reasoning_effort"] = re
	} else if re, ok := params["reasoningEffort"].(string); ok && re != "" {
		body["reasoning_effort"] = re
	}

	if model == "" || strings.HasPrefix(model, "z-ai/glm-") {
		body["enable_thinking"] = true
	}

	for _, key := range passThroughKeys {
		if val, ok := params[key]; ok {
			body[key] = val
		}
	}

	return body
}

func clineHeaders(token, sessionID string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("X-Task-ID", sessionID)

	cfg := getProxyConfig()
	for k, v := range cfg.Headers {
		h.Set(k, v)
	}

	return h
}

// clineAPIError 保留上游或代理侧可返回给客户端的 HTTP 状态码。
type clineAPIError struct {
	StatusCode int
	Message    string
	Cause      error
}

func (e *clineAPIError) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return e.Message + ": " + e.Cause.Error()
	}
	return e.Message
}

func (e *clineAPIError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func newClineAPIError(status int, message string, cause error) error {
	return &clineAPIError{StatusCode: status, Message: message, Cause: cause}
}

// callClineAPI 保留给没有客户端请求上下文的内部调用；HTTP handler 使用
// callClineAPIContext，把客户端断开传递给上游请求。
func callClineAPI(params map[string]any, stream bool) (*http.Response, *Account, error) {
	return callClineAPIContext(context.Background(), params, stream)
}

// callClineAPIContext 调用 Cline 上游，最多总计尝试 3 次。
//
// 429（包括 HTTP 500 中嵌入 INFERENCE_CAP_ERROR）会冷却当前账号并切换账号；
// 5xx 和网络错误会短暂冷却当前账号后重试；客户端取消不会被伪装成上游错误。
func callClineAPIContext(ctx context.Context, params map[string]any, stream bool) (*http.Response, *Account, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	body := buildUpstreamBody(params, stream)
	sessionID, _ := body["session_id"].(string)
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal body: %w", err)
	}

	attempted := map[string]bool{}
	transientAccounts := make([]*Account, 0, clineMaxAttempts)
	transientAccountIDs := map[string]bool{}
	var lastAcc *Account
	var lastErr error

	addTransientAccount := func(acc *Account) {
		if acc == nil {
			return
		}
		for _, existing := range transientAccounts {
			if existing.AccountID == acc.AccountID {
				return
			}
		}
		transientAccounts = append(transientAccounts, acc)
		transientAccountIDs[acc.AccountID] = true
	}

	waitRetry := func(attempt int) error {
		if clineRetryDelay <= 0 || attempt >= clineMaxAttempts {
			return nil
		}
		delay := time.Duration(attempt) * clineRetryDelay
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}

	for attempt := 1; attempt <= clineMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, lastAcc, err
		}

		acc := pickAccountExcluding(attempted)
		if acc == nil && len(transientAccounts) > 0 {
			// 只有所有未尝试账号都不可用时，才复用发生网络/5xx 的账号；
			// 429 账号不会进入这个列表，避免额度耗尽后反复撞同一账号。
			acc = transientAccounts[(attempt-1)%len(transientAccounts)]
		}
		if acc == nil {
			if lastErr != nil {
				return nil, lastAcc, lastErr
			}
			return nil, nil, newClineAPIError(http.StatusServiceUnavailable,
				"no active accounts available: "+describePoolStatus(), nil)
		}
		attempted[acc.AccountID] = true
		lastAcc = acc

		token, err := ensureAccountToken(acc)
		if err != nil {
			if ctx.Err() != nil {
				return nil, acc, ctx.Err()
			}
			status := http.StatusServiceUnavailable
			if errors.Is(err, cline.ErrRefreshTokenInvalid) {
				status = http.StatusUnauthorized
			} else {
				markAccountCooldown(acc, "token refresh transient error: "+err.Error(), clineTransientCooldown)
				addTransientAccount(acc)
			}
			lastErr = newClineAPIError(status, fmt.Sprintf("account %s token failed", acc.Email), err)
			log.Printf("  upstream: account=%s token failed attempt=%d/%d: %v",
				truncateEmail(acc.Email), attempt, clineMaxAttempts, err)
			if err := waitRetry(attempt); err != nil {
				return nil, acc, err
			}
			continue
		}

		toolCount := 0
		if tools, ok := params["tools"]; ok {
			if t, ok := tools.([]any); ok {
				toolCount = len(t)
			}
		}
		log.Printf("  upstream: account=%s attempt=%d/%d stream=%v tools=%d msgs=%d max_tokens=%v effort=%v",
			truncateEmail(acc.Email), attempt, clineMaxAttempts, stream, toolCount, getMsgCount(params), body["max_tokens"], body["reasoning_effort"])

		doRequest := func(accessToken string) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, "POST", cline.ClineAPIBase+"/chat/completions", bytes.NewReader(bodyJSON))
			if err != nil {
				return nil, fmt.Errorf("create request: %w", err)
			}
			req.Header = clineHeaders(accessToken, sessionID)
			req.Header.Set("Accept", "text/event-stream")
			return kit.HTTPClient.Do(req)
		}

		resp, err := doRequest(token)
		if err != nil {
			if ctx.Err() != nil {
				return nil, acc, ctx.Err()
			}
			markAccountCooldown(acc, "network error: "+err.Error(), clineTransientCooldown)
			addTransientAccount(acc)
			lastErr = newClineAPIError(http.StatusBadGateway,
				fmt.Sprintf("upstream request for account %s", acc.Email), err)
			log.Printf("  upstream: account=%s network error attempt=%d/%d: %v",
				truncateEmail(acc.Email), attempt, clineMaxAttempts, err)
			if err := waitRetry(attempt); err != nil {
				return nil, acc, err
			}
			continue
		}

		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			if refreshErr := refreshAccountToken(acc); refreshErr != nil {
				if errors.Is(refreshErr, cline.ErrRefreshTokenInvalid) {
					poolMu.Lock()
					acc.Status = "expired"
					savePoolLocked()
					poolMu.Unlock()
				}
				status := http.StatusServiceUnavailable
				if errors.Is(refreshErr, cline.ErrRefreshTokenInvalid) {
					status = http.StatusUnauthorized
				} else {
					markAccountCooldown(acc, "token refresh transient error: "+refreshErr.Error(), clineTransientCooldown)
					addTransientAccount(acc)
				}
				lastErr = newClineAPIError(status,
					fmt.Sprintf("account %s refresh failed", acc.Email), refreshErr)
				log.Printf("  upstream: account=%s refresh failed after 401 attempt=%d/%d: %v",
					truncateEmail(acc.Email), attempt, clineMaxAttempts, refreshErr)
				if err := waitRetry(attempt); err != nil {
					return nil, acc, err
				}
				continue
			}

			resp, err = doRequest(acc.AccessToken)
			if err != nil {
				if ctx.Err() != nil {
					return nil, acc, ctx.Err()
				}
				markAccountCooldown(acc, "network error after token refresh: "+err.Error(), clineTransientCooldown)
				addTransientAccount(acc)
				lastErr = newClineAPIError(http.StatusBadGateway,
					fmt.Sprintf("upstream request for account %s after token refresh", acc.Email), err)
				if err := waitRetry(attempt); err != nil {
					return nil, acc, err
				}
				continue
			}
			if resp.StatusCode == http.StatusUnauthorized {
				resp.Body.Close()
				poolMu.Lock()
				acc.Status = "expired"
				acc.LastReason = "upstream returned 401 after token refresh"
				savePoolLocked()
				poolMu.Unlock()
				lastErr = newClineAPIError(http.StatusUnauthorized,
					fmt.Sprintf("account %s token expired permanently", acc.Email), nil)
				if err := waitRetry(attempt); err != nil {
					return nil, acc, err
				}
				continue
			}
		}

		if resp.StatusCode == http.StatusOK {
			if transientAccountIDs[acc.AccountID] {
				restoreAccountAfterTransient(acc)
			}
			bumpUsage(acc)
			return resp, acc, nil
		}

		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		bodyText := string(bodyBytes)
		reason := kit.Truncate(bodyText, 500)
		effectiveStatus := resp.StatusCode
		if resp.StatusCode == http.StatusTooManyRequests || isProbeRateLimitText(bodyText) {
			effectiveStatus = http.StatusTooManyRequests
		}

		if effectiveStatus == http.StatusTooManyRequests {
			duration := parseInferenceCapDuration(bodyText)
			if duration <= 0 {
				duration = parseRetryAfter(resp.Header.Get("Retry-After"))
			}
			markAccountCooldown(acc, fmt.Sprintf("429: %s", reason), duration)
			log.Printf("  account %s cooldown %v (status=%d reason=%s)",
				truncateEmail(acc.Email), duration, resp.StatusCode, reason)
			lastErr = newClineAPIError(http.StatusTooManyRequests,
				fmt.Sprintf("API %d: %s", effectiveStatus, reason), nil)
			if err := waitRetry(attempt); err != nil {
				return nil, acc, err
			}
			continue
		}

		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooEarly {
			markAccountCooldown(acc, fmt.Sprintf("API %d: %s", resp.StatusCode, reason), clineTransientCooldown)
			addTransientAccount(acc)
			lastErr = newClineAPIError(effectiveStatus,
				fmt.Sprintf("API %d: %s", effectiveStatus, reason), nil)
			if err := waitRetry(attempt); err != nil {
				return nil, acc, err
			}
			continue
		}

		return nil, acc, newClineAPIError(effectiveStatus,
			fmt.Sprintf("API %d: %s", effectiveStatus, reason), nil)
	}

	if lastErr != nil {
		return nil, lastAcc, lastErr
	}
	return nil, lastAcc, newClineAPIError(http.StatusServiceUnavailable,
		"upstream request failed after retries", nil)
}

func extractUsageTokens(u map[string]any) (int64, int64) {
	var pt, ct float64
	if v, ok := u["prompt_tokens"].(float64); ok {
		pt = v
	}
	if v, ok := u["completion_tokens"].(float64); ok {
		ct = v
	}
	return int64(pt), int64(ct)
}

func setRequestLogMetaFromUsage(r *http.Request, acc *Account, u map[string]any, params map[string]any) {
	input, output := extractUsageTokens(u)
	if input+output <= 0 && params != nil {
		input = int64(estimateJSON(params))
	}
	setRequestLogMeta(r, acc.Email, input, output)
}

// accountUsageFn 构造账号 token 记账回调：从上游 usage 提取
// prompt_tokens + completion_tokens，计入该账号今日/累计消耗。
func accountUsageFn(acc *Account, params map[string]any) func(map[string]any) {
	return func(u map[string]any) {
		input, output := extractUsageTokens(u)
		tokens := input + output
		if tokens <= 0 && params != nil {
			// 上游未返回 usage 时用入站请求估算兜底（与 zen 统计一致）
			tokens = int64(estimateJSON(params))
		}
		recordAccountTokens(acc, tokens)
	}
}

func truncateEmail(email string) string {
	if len(email) <= 12 {
		return email
	}
	parts := splitEmail(email)
	if len(parts) == 2 && len(parts[0]) > 3 {
		return parts[0][:3] + "***@" + parts[1]
	}
	if len(email) > 12 {
		return email[:8] + "..."
	}
	return email
}

func splitEmail(email string) []string {
	for i := 0; i < len(email); i++ {
		if email[i] == '@' {
			return []string{email[:i], email[i+1:]}
		}
	}
	return []string{email}
}

func getMsgCount(params map[string]any) int {
	if msgs, ok := params["messages"].([]any); ok {
		return len(msgs)
	}
	return 0
}

func handleStreamResponseWithUsage(w http.ResponseWriter, upstream *http.Response, onUsage func(map[string]any)) {
	handleStreamResponseWithUsageContext(context.Background(), w, upstream, onUsage)
}

// handleStreamResponseWithUsageContext 转发 Chat Completions SSE，同时提供心跳、
// 客户端取消和上游异常 EOF 处理。正常结束必须看到上游 [DONE]。
func handleStreamResponseWithUsageContext(ctx context.Context, w http.ResponseWriter, upstream *http.Response, onUsage func(map[string]any)) {
	defer upstream.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("  streaming not supported for client")
		return
	}

	writeHeartbeat := func() {
		_, _ = io.WriteString(w, ": keep-alive\n\n")
		flusher.Flush()
	}
	writeFailure := func(message string) {
		if ctx.Err() != nil {
			return
		}
		payload, _ := json.Marshal(map[string]any{
			"error": map[string]string{
				"message": message,
				"type":    "api_error",
			},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		flusher.Flush()
	}

	lines, stopLines := readStreamLines(upstream.Body)
	defer stopLines()
	idleTimer := newStreamIdleTimer()
	defer stopStreamIdleTimer(idleTimer)
	var heartbeat <-chan time.Time
	var ticker *time.Ticker
	if streamHeartbeatInterval > 0 {
		ticker = time.NewTicker(streamHeartbeatInterval)
		defer ticker.Stop()
		heartbeat = ticker.C
	}
	var idle <-chan time.Time
	if idleTimer != nil {
		idle = idleTimer.C
	}

	sawDone := false
	streamErr := error(nil)
	for {
		select {
		case <-streamContextDone(ctx):
			_ = upstream.Body.Close()
			return
		case <-heartbeat:
			writeHeartbeat()
		case <-idle:
			streamErr = fmt.Errorf("upstream stream idle for %s", streamReadIdleTimeout)
			_ = upstream.Body.Close()
		case result, ok := <-lines:
			if !ok {
				if streamErr == nil && !sawDone {
					streamErr = io.ErrUnexpectedEOF
				}
				break
			}
			if result.line != "" {
				resetStreamIdleTimer(idleTimer)
			}
			line := strings.TrimRight(result.line, "\r\n")
			if line != "" {
				if strings.HasPrefix(line, "data:") {
					payload := strings.TrimSpace(line[5:])
					if payload == "" || payload == "[DONE]" {
						if payload == "[DONE]" {
							sawDone = true
						}
						_, _ = io.WriteString(w, line+"\n\n")
						flusher.Flush()
					} else {
						// Try to normalize the response.
						var obj map[string]any
						if err := json.Unmarshal([]byte(payload), &obj); err == nil {
							if onUsage != nil {
								if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
									onUsage(u)
								}
							}
							if _, hasChoices := obj["choices"]; !hasChoices {
								if data, ok := obj["data"].([]any); ok && len(data) > 0 {
									if dm, ok := data[0].(map[string]any); ok {
										if _, hasChoices := dm["choices"]; hasChoices {
											obj = dm
										}
									}
								}
							}
							// Some Cline responses wrap in {data: {...}}.
							if data, ok := obj["data"]; ok {
								if d, ok := data.(map[string]any); ok {
									if _, hasChoices := d["choices"]; hasChoices {
										obj = d
									}
									if _, hasID := d["id"]; hasID {
										obj = d
									}
								}
							}
							normalized := normalizeOpenAIResponse(obj)
							if normBytes, err := json.Marshal(normalized); err == nil {
								_, _ = fmt.Fprintf(w, "data: %s\n\n", normBytes)
								flusher.Flush()
							} else {
								_, _ = io.WriteString(w, line+"\n\n")
								flusher.Flush()
							}
						} else {
							_, _ = io.WriteString(w, line+"\n\n")
							flusher.Flush()
						}
					}
				} else {
					_, _ = io.WriteString(w, line+"\n\n")
					flusher.Flush()
				}
			}

			if result.err != nil {
				if result.err != io.EOF && result.err != bufio.ErrBufferFull {
					streamErr = result.err
				}
				break
			}
		}
		if streamErr != nil || sawDone {
			break
		}
	}

	if !sawDone {
		if streamErr == nil {
			streamErr = io.ErrUnexpectedEOF
		}
		log.Printf("  upstream stream failed before [DONE]: %v", streamErr)
		writeFailure("upstream stream ended before [DONE]: " + streamErr.Error())
		return
	}
}

func handleNonStreamResponseWithUsage(w http.ResponseWriter, upstream *http.Response, onUsage func(map[string]any)) {
	var raw map[string]any
	if err := json.NewDecoder(upstream.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return
	}

	if onUsage != nil {
		if u, ok := raw["usage"].(map[string]any); ok && len(u) > 0 {
			onUsage(u)
		}
	}

	// Some Cline responses wrap in {data: {...}}
	out := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			out = d
		}
	}

	out = normalizeOpenAIResponse(out)

	if msg, ok := getNested(out, "choices", 0, "message").(map[string]any); ok {
		tc, _ := msg["tool_calls"].([]any)
		content, _ := msg["content"].(string)
		log.Printf("  nonstream finish=%v tool_calls=%d content_len=%d",
			getNested(out, "choices", 0, "finish_reason"),
			len(tc), len(content))
	}

	writeJSON(w, http.StatusOK, out)
}

func collectStreamResponse(upstream *http.Response) (map[string]any, error) {
	return collectStreamResponseContext(context.Background(), upstream)
}

func collectStreamResponseContext(ctx context.Context, upstream *http.Response) (map[string]any, error) {
	defer upstream.Body.Close()
	var (
		model        string
		content      strings.Builder
		finishReason string
		usage        map[string]any
		toolCalls    []any
		toolCallIdx  = -1
		curToolCall  map[string]any
		curArgs      strings.Builder
	)

	lines, stopLines := readStreamLines(upstream.Body)
	defer stopLines()
	idleTimer := newStreamIdleTimer()
	defer stopStreamIdleTimer(idleTimer)
	var idle <-chan time.Time
	if idleTimer != nil {
		idle = idleTimer.C
	}

	sawDone := false
	streamErr := error(nil)
	for {
		select {
		case <-streamContextDone(ctx):
			_ = upstream.Body.Close()
			return nil, ctx.Err()
		case <-idle:
			streamErr = fmt.Errorf("upstream stream idle for %s", streamReadIdleTimeout)
			_ = upstream.Body.Close()
		case result, ok := <-lines:
			if !ok {
				if streamErr == nil && !sawDone {
					streamErr = io.ErrUnexpectedEOF
				}
				break
			}
			if result.line != "" {
				resetStreamIdleTimer(idleTimer)
			}
			line := strings.TrimRight(result.line, "\r\n")
			if strings.HasPrefix(line, "data:") {
				payload := strings.TrimSpace(line[5:])
				if payload == "" || payload == "[DONE]" {
					if payload == "[DONE]" {
						sawDone = true
					}
				} else {
					var obj map[string]any
					if json.Unmarshal([]byte(payload), &obj) == nil {
						if data, ok := obj["data"]; ok {
							if d, ok := data.(map[string]any); ok {
								obj = d
							}
						}
						if errPayload, ok := obj["error"]; ok {
							encoded, _ := json.Marshal(errPayload)
							streamErr = fmt.Errorf("upstream error: %s", encoded)
							break
						}
						if m, ok := obj["model"].(string); ok && m != "" {
							model = m
						}
						if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
							usage = u
						}
						choices, _ := getNested(obj, "choices").([]any)
						if len(choices) > 0 {
							choice, _ := choices[0].(map[string]any)
							if choice != nil {
								delta, _ := choice["delta"].(map[string]any)
								if delta == nil {
									delta = choice
								}
								if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
									finishReason = fr
								}
								if c, ok := delta["content"].(string); ok && c != "" {
									content.WriteString(c)
								}
								if tcRaw, ok := delta["tool_calls"].([]any); ok {
									for _, tc := range tcRaw {
										tcMap, _ := tc.(map[string]any)
										if tcMap == nil {
											continue
										}
										idx := 0
										if i, ok := tcMap["index"].(float64); ok {
											idx = int(i)
										}
										if idx != toolCallIdx {
											if curToolCall != nil {
												curToolCall["function"].(map[string]any)["arguments"] = curArgs.String()
												toolCalls = append(toolCalls, curToolCall)
											}
											curToolCall = map[string]any{
												"id":       tcMap["id"],
												"type":     "function",
												"function": map[string]any{"name": "", "arguments": ""},
											}
											curArgs.Reset()
											toolCallIdx = idx
										}
										if fn, ok := tcMap["function"].(map[string]any); ok {
											if n, ok := fn["name"].(string); ok && n != "" {
												curToolCall["function"].(map[string]any)["name"] = n
											}
											if a, ok := fn["arguments"].(string); ok && a != "" {
												curArgs.WriteString(a)
											}
										}
									}
								}
							}
						}
					}
				}
			}
			if result.err != nil {
				if result.err != io.EOF && result.err != bufio.ErrBufferFull {
					streamErr = result.err
				}
				break
			}
		}
		if streamErr != nil || sawDone {
			break
		}
	}
	if !sawDone {
		if streamErr == nil {
			streamErr = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("upstream stream ended before [DONE]: %w", streamErr)
	}

	if curToolCall != nil {
		curToolCall["function"].(map[string]any)["arguments"] = curArgs.String()
		toolCalls = append(toolCalls, curToolCall)
	}

	message := map[string]any{
		"role":    "assistant",
		"content": content.String(),
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	choice := map[string]any{
		"index":         0,
		"message":       message,
		"finish_reason": finishReason,
	}
	out := map[string]any{
		"id":      "chatcmpl_" + fmt.Sprintf("%x", time.Now().UnixMilli()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{choice},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return out, nil
}

func modelNeedsStream(modelID string) bool {
	initModelsCache()
	modelsMu.Lock()
	defer modelsMu.Unlock()
	if m, ok := modelsCache[modelID]; ok && m.RequiresStream {
		return true
	}
	return false
}

// Anthropic Messages API support
type anthropicMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type toolAccumulator struct {
	index   int
	id      string
	name    string
	args    string
	emitted bool
}

type anthropicReq struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	Messages    []anthropicMsg  `json:"messages"`
	System      json.RawMessage `json:"system,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
	TopP        float64         `json:"top_p,omitempty"`
	TopK        int             `json:"top_k,omitempty"`
	Stop        json.RawMessage `json:"stop_sequences,omitempty"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Extra       map[string]any  `json:"-"`
}

func loadOverrideContent() string {
	data, err := os.ReadFile("override.md")
	if err != nil {
		// override.md 是可选功能，文件不存在时静默使用客户端自带提示词
		return ""
	}
	content := strings.TrimSpace(string(data))
	if content != "" {
		log.Printf("  using override.md as system prompt (%d bytes)", len(content))
	} else {
		log.Printf("  override.md is empty, using client system prompt")
	}
	return content
}

func extractStringContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Try array of content blocks
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := []string{}
		for _, b := range blocks {
			if b["type"] == "text" {
				if t, ok := b["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func anthropicToolsToOpenAI(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		if tMap, ok := t.(map[string]any); ok {
			// Already in OpenAI format
			if tMap["type"] == "function" {
				out = append(out, t)
				continue
			}
			// Convert Anthropic format to OpenAI
			oai := map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        tMap["name"],
					"description": tMap["description"],
					"parameters":  tMap["input_schema"],
				},
			}
			out = append(out, oai)
		}
	}
	return out
}

func anthropicToOpenAI(req anthropicReq) map[string]any {
	openAI := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"stream":     req.Stream,
		"messages":   []any{},
	}
	if req.Temperature != 0 {
		openAI["temperature"] = req.Temperature
	}
	if req.TopP != 0 {
		openAI["top_p"] = req.TopP
	}
	// Convert Anthropic tools to OpenAI format
	if req.Tools != nil {
		var toolsArr []any
		if err := json.Unmarshal(req.Tools, &toolsArr); err == nil {
			openAI["tools"] = anthropicToolsToOpenAI(toolsArr)
		}
	}
	if req.ToolChoice != nil {
		openAI["tool_choice"] = req.ToolChoice
	}

	msgs := []any{}

	// System prompt: use override.md if it exists, otherwise use Anthropic's system field
	sysContent := loadOverrideContent()
	if sysContent == "" && req.System != nil {
		sysContent = extractStringContent(req.System)
	}
	if sysContent != "" {
		log.Printf("  system prompt: %d bytes (from override.md)", len(sysContent))
		msgs = append(msgs, map[string]any{"role": "system", "content": sysContent})
	}

	for _, m := range req.Messages {
		switch c := m.Content.(type) {
		case string:
			msgs = append(msgs, map[string]any{"role": m.Role, "content": c})
		case []any:
			textParts := []string{}
			var toolCalls []any
			var toolResults []map[string]any

			for _, block := range c {
				if b, ok := block.(map[string]any); ok {
					switch b["type"] {
					case "text":
						if t, ok := b["text"].(string); ok {
							textParts = append(textParts, t)
						}
					case "image":
						// skip images
					case "tool_use":
						argsStr := "{}"
						if input, ok := b["input"]; ok && input != nil {
							if s, ok := input.(string); ok {
								argsStr = s
							} else if bts, err := json.Marshal(input); err == nil {
								argsStr = string(bts)
							}
						}
						tc := map[string]any{
							"id":   b["id"],
							"type": "function",
							"function": map[string]any{
								"name":      b["name"],
								"arguments": argsStr,
							},
						}
						toolCalls = append(toolCalls, tc)
					case "tool_result":
						toolCallID, _ := b["tool_use_id"].(string)
						if toolCallID == "" {
							continue
						}
						toolResults = append(toolResults, map[string]any{
							"role":         "tool",
							"content":      anthropicContentToString(b["content"]),
							"tool_call_id": toolCallID,
						})
					}
				}
			}

			if m.Role == "assistant" && len(toolCalls) > 0 {
				msg := map[string]any{
					"role":       "assistant",
					"content":    strings.Join(textParts, "\n"),
					"tool_calls": toolCalls,
				}
				msgs = append(msgs, msg)
				log.Printf("  anthropic req: assistant tool_calls=%d", len(toolCalls))
			} else if m.Role == "user" && len(toolResults) > 0 {
				for _, tr := range toolResults {
					msgs = append(msgs, tr)
					content, _ := tr["content"].(string)
					id, _ := tr["tool_call_id"].(string)
					log.Printf("  anthropic req: tool_result id=%s content_len=%d prefix=%s", id, len(content), kit.Truncate(content, 400))
				}
			} else {
				content := strings.Join(textParts, "\n")
				msgs = append(msgs, map[string]any{"role": m.Role, "content": content})
			}
		}
	}

	openAI["messages"] = msgs
	return openAI
}

// parseToolArgs 解析工具调用参数 JSON，带容错修复
func parseToolArgs(raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}, nil
	}
	// 清理杂散引号前缀：上游流式输出偶发 "" 前缀（如 ""{"file_path":...}）
	for strings.HasPrefix(raw, `""`) {
		raw = strings.TrimPrefix(raw, `""`)
	}
	raw = strings.TrimSpace(raw)
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		if v == nil {
			return map[string]any{}, nil
		}
		return v, nil
	}
	// 整体被 JSON 字符串包裹（"{\"file_path\": ...}"）时，解包字符串后再解析
	if strings.HasPrefix(raw, `"`) && strings.HasSuffix(raw, `"`) && len(raw) >= 2 {
		var s string
		if json.Unmarshal([]byte(raw), &s) == nil {
			var v2 any
			if json.Unmarshal([]byte(s), &v2) == nil && v2 != nil {
				return v2, nil
			}
		}
	}
	fixed := raw
	if strings.HasPrefix(fixed, "{") && !strings.HasSuffix(fixed, "}") {
		fixed += "}"
	} else if strings.HasPrefix(fixed, "[") && !strings.HasSuffix(fixed, "]") {
		fixed += "]"
	}
	if strings.HasSuffix(fixed, ",") {
		fixed = strings.TrimRight(fixed, ",") + "}"
	}
	if err := json.Unmarshal([]byte(fixed), &v); err == nil && v != nil {
		return v, nil
	}
	// 最终兜底：从杂散内容中提取首个 JSON 对象/数组
	if i := strings.IndexAny(raw, "{["); i >= 0 {
		openCh := raw[i]
		closeCh := byte('}')
		if openCh == '[' {
			closeCh = ']'
		}
		if j := strings.LastIndex(raw, string(closeCh)); j > i {
			sub := raw[i : j+1]
			if json.Unmarshal([]byte(sub), &v) == nil && v != nil {
				return v, nil
			}
		}
	}
	return nil, fmt.Errorf("invalid json: %s", kit.Truncate(raw, 120))
}

// extractToolSchemas 从 Anthropic 请求的 tools 定义中解析每个工具的 input_schema 属性集合，
// 用于转发 tool_use 时裁剪 input，避免多余字段触发客户端校验失败。
func extractToolSchemas(tools json.RawMessage) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	if len(tools) == 0 {
		return out
	}
	var arr []map[string]any
	if err := json.Unmarshal(tools, &arr); err != nil {
		return out
	}
	for _, t := range arr {
		name, _ := t["name"].(string)
		if name == "" {
			continue
		}
		schema, _ := t["input_schema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		var propNames []string
		for k := range props {
			propNames = append(propNames, k)
		}
		var required []string
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				if s, ok := r.(string); ok {
					required = append(required, s)
				}
			}
		}
		log.Printf("  tool schema: name=%s properties=%v required=%v", name, propNames, required)
		if len(props) == 0 {
			continue
		}
		allowed := map[string]bool{}
		for k := range props {
			allowed[k] = true
		}
		out[name] = allowed
	}
	return out
}

// filterToolInput 将工具参数裁剪到客户端 schema 允许的字段内；
// 找不到 schema 或过滤后为空时保留原参数，避免丢参数。
func filterToolInput(name string, input map[string]any, schemas map[string]map[string]bool) map[string]any {
	allowed, ok := schemas[name]
	if !ok || len(allowed) == 0 {
		return input
	}
	out := map[string]any{}
	for k, v := range input {
		if allowed[k] {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return input
	}
	return out
}

// anthropicContentToString 将 Anthropic content（字符串或块数组）转为纯文本
func anthropicContentToString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if arr, ok := v.([]any); ok {
		parts := []string{}
		for _, it := range arr {
			if b, ok := it.(map[string]any); ok {
				if t, ok := b["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func openAIToAnthropic(openAI map[string]any) map[string]any {
	out := map[string]any{
		"id":    "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli()),
		"type":  "message",
		"role":  "assistant",
		"model": getNested(openAI, "model"),
	}

	choices := getNested(openAI, "choices")
	if choices == nil {
		out["content"] = []any{map[string]any{"type": "text", "text": ""}}
		out["stop_reason"] = "end_turn"
		out["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0}
		return out
	}

	text := ""
	choice0, ok := getNested(openAI, "choices", 0).(map[string]any)
	if !ok {
		out["content"] = []any{map[string]any{"type": "text", "text": text}}
		out["stop_reason"] = "end_turn"
		out["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0}
		return out
	}
	msg, _ := choice0["message"].(map[string]any)
	if msg == nil {
		msg, _ = choice0["delta"].(map[string]any)
	}
	if msg != nil {
		if c, ok := msg["content"].(string); ok {
			text = sanitizeContent(c)
		}
	}

	contentBlocks := []any{map[string]any{"type": "text", "text": text}}

	// Convert tool_calls to Anthropic tool_use blocks
	if msg != nil {
		if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
			contentBlocks = []any{}
			if text != "" {
				contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": text})
			}
			for _, tcItem := range tc {
				if tcMap, ok := tcItem.(map[string]any); ok {
					funcData, _ := tcMap["function"].(map[string]any)
					if funcData == nil {
						continue
					}
					input := funcData["arguments"]
					// OpenAI arguments is a JSON string; Anthropic expects an object
					if argsStr, ok := input.(string); ok {
						var argsObj any
						if json.Unmarshal([]byte(argsStr), &argsObj) == nil {
							input = argsObj
						}
					}
					if input == nil {
						input = map[string]any{}
					}
					id, _ := tcMap["id"].(string)
					if id == "" {
						id = fmt.Sprintf("toolu_%x_%d", time.Now().UnixMilli(), len(contentBlocks))
					}
					name, _ := funcData["name"].(string)
					if name == "" {
						continue
					}
					block := map[string]any{
						"type":  "tool_use",
						"id":    id,
						"name":  name,
						"input": input,
					}
					contentBlocks = append(contentBlocks, block)
				}
			}
		}
	}

	out["content"] = contentBlocks

	switch getNested(openAI, "choices", 0, "finish_reason") {
	case "stop":
		out["stop_reason"] = "end_turn"
	case "length":
		out["stop_reason"] = "max_tokens"
	case "tool_calls":
		out["stop_reason"] = "tool_use"
	default:
		out["stop_reason"] = "end_turn"
	}

	usage := map[string]any{}
	if u := getNested(openAI, "usage"); u != nil {
		if um, ok := u.(map[string]any); ok {
			usage["input_tokens"] = um["prompt_tokens"]
			usage["output_tokens"] = um["completion_tokens"]
		}
	}
	out["usage"] = usage

	return out
}

func handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return
	}

	var req anthropicReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		return
	}

	if len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "messages is required", "type": "parse_error"},
		})
		return
	}

	toolSchemas := extractToolSchemas(req.Tools)
	if len(toolSchemas) > 0 {
		log.Printf("  anthropic tools: %d schemas", len(toolSchemas))
	}

	if req.MaxTokens == 0 {
		req.MaxTokens = defaultMaxTokens
	}

	openAIReq := anthropicToOpenAI(req)

	log.Printf("  anthropic: model=%s stream=%v msgs=%d", req.Model, req.Stream, len(req.Messages))

	// zen 免费模型路由
	if route := routeModel(req.Model); route == "zen" {
		handleZenAnthropic(w, r, req, openAIReq, toolSchemas)
		return
	} else if route == "reject" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": fmt.Sprintf("model %q is a paid zen model; only free zen models are proxied", req.Model), "type": "invalid_request_error"},
		})
		return
	}

	activeCount := 0
	p := loadPool()
	for _, a := range p.Accounts {
		if a.Status == "active" {
			activeCount++
		}
	}

	if activeCount == 0 && len(p.Accounts) == 0 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]string{
				"message": "No accounts in pool",
				"type":    "auth_error",
			},
		})
		return
	}

	upstreamStream := req.Stream
	if !req.Stream && modelNeedsStream(normalizeRequestModel(req.Model)) {
		upstreamStream = true
		log.Printf("  anthropic model %s requires stream: forcing upstream stream, will aggregate", req.Model)
	}

	resp, acc, err := callClineAPIContext(r.Context(), openAIReq, upstreamStream)
	if err != nil {
		log.Printf("  anthropic api error: %v", err)
		writeClineAPIError(w, err)
		return
	}
	defer resp.Body.Close()

	usageFn := accountUsageFn(acc, openAIReq)
	usageFnWithLog := func(u map[string]any) {
		setRequestLogMetaFromUsage(r, acc, u, openAIReq)
		usageFn(u)
	}

	if req.Stream {
		handleAnthropicStreamWithUsageContext(r.Context(), w, resp, normalizeRequestModel(req.Model), toolSchemas, usageFnWithLog)
		return
	}

	if upstreamStream {
		out, err := collectStreamResponseContext(r.Context(), resp)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "parse_error"},
			})
			return
		}
		if u, ok := out["usage"].(map[string]any); ok && len(u) > 0 {
			usageFnWithLog(u)
		}
		out = normalizeOpenAIResponse(out)
		anthropicResp := openAIToAnthropic(out)
		if tc, ok := getNested(out, "choices", 0, "message", "tool_calls").([]any); ok && len(tc) > 0 {
			anthropicResp["stop_reason"] = "tool_use"
		}
		writeJSON(w, http.StatusOK, anthropicResp)
		return
	}

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
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
	out = normalizeOpenAIResponse(out)
	anthropicResp := openAIToAnthropic(out)

	if tc, ok := getNested(out, "choices", 0, "message", "tool_calls").([]any); ok && len(tc) > 0 {
		anthropicResp["stop_reason"] = "tool_use"
	}

	writeJSON(w, http.StatusOK, anthropicResp)
}

// handleZenAnthropic Anthropic Messages 请求路由到 zen 免费模型上游
func handleZenAnthropic(w http.ResponseWriter, r *http.Request, req anthropicReq, openAIReq map[string]any, toolSchemas map[string]map[string]bool) {
	cfg := getZenConfig()
	if !cfg.Enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": "zen upstream disabled in /admin/ settings", "type": "api_error"},
		})
		return
	}
	zm, ok := resolveZenFreeModel(req.Model)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": fmt.Sprintf("model %q is not a free zen model", req.Model), "type": "invalid_request_error"},
		})
		return
	}
	isStream := req.Stream
	tracker := newZenStatsTracker(zenStatsRecord{
		TS:           time.Now().UnixMilli(),
		Upstream:     "zen",
		Model:        zm.ID,
		Stream:       isStream,
		PromptTokens: estimateJSON(openAIReq),
	})

	sid := requestSessionID(openAIReq, r.Header)
	out := maybeCompact(openAIReq, zm, sid)
	tracker.rec.Compacted = out.changed
	tracker.rec.CompactionTokens = out.compactTokens
	if out.changed {
		log.Printf("  anthropic zen: %s", out.note)
	}

	resp, rateLimited, err := callZenAPI(openAIReq, isStream)
	if err != nil {
		log.Printf("  anthropic zen api error: %v", err)
		tracker.rec.RateLimited = rateLimited
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		tracker.finish(false, http.StatusBadGateway)
		return
	}
	tracker.rec.RateLimited = rateLimited
	defer resp.Body.Close()
	tracker.rec.Status = resp.StatusCode

	usageFn := func(u map[string]any) {
		if ct, ok := u["completion_tokens"].(float64); ok {
			tracker.rec.CompletionTokens = int(ct)
		}
	}

	if isStream {
		handleAnthropicStreamWithUsageContext(r.Context(), w, resp, zm.ID, toolSchemas, usageFn)
		tracker.finish(true, resp.StatusCode)
		return
	}

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "parse_error"},
		})
		tracker.finish(false, http.StatusInternalServerError)
		return
	}
	if u, ok := raw["usage"].(map[string]any); ok && len(u) > 0 {
		usageFn(u)
	}
	chatOut := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			chatOut = d
		}
	}
	chatOut = normalizeOpenAIResponse(chatOut)
	anthropicResp := openAIToAnthropic(chatOut)
	if tc, ok := getNested(chatOut, "choices", 0, "message", "tool_calls").([]any); ok && len(tc) > 0 {
		anthropicResp["stop_reason"] = "tool_use"
	}
	writeJSON(w, http.StatusOK, anthropicResp)
	tracker.finish(true, resp.StatusCode)
}

func handleAnthropicStreamWithUsage(w http.ResponseWriter, upstream *http.Response, modelName string, toolSchemas map[string]map[string]bool, onUsage func(map[string]any)) {
	handleAnthropicStreamWithUsageContext(context.Background(), w, upstream, modelName, toolSchemas, onUsage)
}

func handleAnthropicStreamWithUsageContext(ctx context.Context, w http.ResponseWriter, upstream *http.Response, modelName string, toolSchemas map[string]map[string]bool, onUsage func(map[string]any)) {
	defer upstream.Body.Close()
	log.Printf("  anthropic stream: starting real-time forward")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	var streamLog *os.File
	if sf, err := os.OpenFile(kit.ResolveDataPath("cline-proxy-stream.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		streamLog = sf
	}
	defer func() {
		if streamLog != nil {
			streamLog.Close()
		}
	}()

	emit := func(event string, data any) {
		d, _ := json.Marshal(data)
		line := fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(d))
		w.Write([]byte(line))
		if streamLog != nil {
			streamLog.WriteString(line)
		}
		flusher.Flush()
	}

	msgID := "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli())
	stopReason := "end_turn"
	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":      msgID,
			"type":    "message",
			"role":    "assistant",
			"content": []any{},
			"model":   modelName,
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
			},
			"stop_reason": nil,
		},
	})

	textIndex := new(int)
	*textIndex = -1
	hasText := false
	pendingTools := map[int]*toolAccumulator{}
	sawDone := false
	streamErr := error(nil)
	emitIndex := 0
	nextIndex := func() int {
		i := emitIndex
		emitIndex++
		return i
	}

	emitToolBlock := func(acc *toolAccumulator) {
		acc.emitted = true
		if acc.name == "" {
			log.Printf("  tool_use missing name, skipping (id=%s)", acc.id)
			return
		}
		idx := nextIndex()
		id := acc.id
		if id == "" {
			id = fmt.Sprintf("toolu_%x_%d", time.Now().UnixMilli(), idx)
			log.Printf("  tool_use missing id, generated %s", id)
		}
		argsObj, err := parseToolArgs(acc.args)
		if err != nil {
			log.Printf("  tool args parse failed for %s: %v (raw: %s)", acc.name, err, kit.Truncate(acc.args, 300))
			argsObj = map[string]any{}
		}
		if inputMap, ok := argsObj.(map[string]any); ok {
			argsObj = filterToolInput(acc.name, inputMap, toolSchemas)
		}
		parsed, _ := json.Marshal(argsObj)
		log.Printf("  tool_use emit: name=%s id=%s input=%s", acc.name, id, string(parsed))
		emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": idx,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    id,
				"name":  acc.name,
				"input": map[string]any{},
			},
		})
		emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": string(parsed),
			},
		})
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": idx,
		})
	}

	processSSELine := func(line string) {
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data:") {
			return
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "" {
			return
		}
		if payload == "[DONE]" {
			sawDone = true
			return
		}

		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			return
		}
		if onUsage != nil {
			if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
				onUsage(u)
			}
		}
		if data, ok := obj["data"]; ok {
			if d, ok := data.(map[string]any); ok {
				obj = d
			}
		}

		if errPayload, ok := obj["error"]; ok {
			errBody, _ := json.Marshal(errPayload)
			log.Printf("  upstream SSE error: %s", string(errBody))
			emit("error", map[string]any{"type": "error", "error": errPayload})
			streamErr = fmt.Errorf("upstream error: %s", errBody)
			return
		}

		choices, _ := getNested(obj, "choices").([]any)
		if len(choices) == 0 {
			return
		}
		choice, _ := choices[0].(map[string]any)
		if choice == nil {
			return
		}

		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			delta = choice
		}

		if c, ok := delta["content"].(string); ok && c != "" {
			if !hasText {
				hasText = true
				*textIndex = nextIndex()
				emit("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": *textIndex,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				})
			}
			emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": *textIndex,
				"delta": map[string]any{
					"type": "text_delta",
					"text": sanitizeContent(c),
				},
			})
		}

		if tcRaw, ok := delta["tool_calls"].([]any); ok {
			for _, tc := range tcRaw {
				tcMap, _ := tc.(map[string]any)
				if tcMap == nil {
					continue
				}
				idx := 0
				if i, ok := tcMap["index"].(float64); ok {
					idx = int(i)
				}
				acc, exists := pendingTools[idx]
				if !exists {
					acc = &toolAccumulator{index: idx}
					pendingTools[idx] = acc
				}
				if id, ok := tcMap["id"].(string); ok && id != "" {
					acc.id = id
				}
				if fn, ok := tcMap["function"].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok && name != "" {
						acc.name = name
					}
					if args, ok := fn["arguments"].(string); ok && args != "" {
						acc.args += args
					} else if argsRaw, ok := fn["arguments"]; ok && argsRaw != nil {
						if bts, err := json.Marshal(argsRaw); err == nil {
							acc.args = string(bts)
						}
					}
				}
			}
		}

		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			switch fr {
			case "length":
				stopReason = "max_tokens"
			case "tool_calls":
				stopReason = "tool_use"
			}
		}
	}

	lines, stopLines := readStreamLines(upstream.Body)
	defer stopLines()
	idleTimer := newStreamIdleTimer()
	defer stopStreamIdleTimer(idleTimer)
	var heartbeat <-chan time.Time
	var ticker *time.Ticker
	if streamHeartbeatInterval > 0 {
		ticker = time.NewTicker(streamHeartbeatInterval)
		defer ticker.Stop()
		heartbeat = ticker.C
	}
	var idle <-chan time.Time
	if idleTimer != nil {
		idle = idleTimer.C
	}

	for {
		select {
		case <-streamContextDone(ctx):
			_ = upstream.Body.Close()
			return
		case <-heartbeat:
			_, _ = io.WriteString(w, ": keep-alive\n\n")
			flusher.Flush()
		case <-idle:
			streamErr = fmt.Errorf("upstream stream idle for %s", streamReadIdleTimeout)
			_ = upstream.Body.Close()
		case result, ok := <-lines:
			if !ok {
				if streamErr == nil && !sawDone {
					streamErr = io.ErrUnexpectedEOF
				}
				break
			}
			if result.line != "" {
				resetStreamIdleTimer(idleTimer)
				processSSELine(result.line)
			}
			if result.err != nil {
				if result.err != io.EOF && result.err != bufio.ErrBufferFull {
					streamErr = result.err
				}
				break
			}
		}
		if streamErr != nil || sawDone {
			break
		}
	}

	if streamErr != nil || !sawDone {
		if streamErr == nil {
			streamErr = io.ErrUnexpectedEOF
		}
		log.Printf("  anthropic upstream stream failed before [DONE]: %v", streamErr)
		emit("error", map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    "api_error",
				"message": "upstream stream failed before [DONE]: " + streamErr.Error(),
			},
		})
		return
	}

	// Stop text block if active
	if hasText {
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": *textIndex,
		})
	}

	// Emit any remaining un-emitted tool blocks
	for _, acc := range pendingTools {
		if !acc.emitted {
			emitToolBlock(acc)
		}
	}

	emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens": 0,
		},
	})

	emit("message_stop", map[string]any{"type": "message_stop"})
	log.Printf("  anthropic stream done: hasText=%v tools=%d reason=%s", hasText, len(pendingTools), stopReason)
}

func normalizeOpenAIResponse(obj map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range obj {
		if k == "provider_metadata" || k == "proxy_metadata" {
			continue
		}
		out[k] = v
	}

	if choices, ok := out["choices"].([]any); ok {
		normalized := make([]any, 0, len(choices))
		for _, ch := range choices {
			if c, ok := ch.(map[string]any); ok {
				nc := make(map[string]any)
				for k, v := range c {
					if k == "provider_metadata" || k == "proxy_metadata" {
						continue
					}
					nc[k] = v
				}
				if msg, ok := nc["message"].(map[string]any); ok {
					nc["message"] = normalizeMessage(msg)
				}
				if delta, ok := nc["delta"].(map[string]any); ok {
					nd := make(map[string]any)
					for k, v := range delta {
						if k == "provider_metadata" || k == "proxy_metadata" {
							continue
						}
						nd[k] = v
					}
					if tc, ok := nd["tool_calls"].([]any); ok && len(tc) > 0 {
						if nd["content"] == nil {
							nd["content"] = ""
						}
					}
					nc["delta"] = nd
				}
				normalized = append(normalized, nc)
			} else {
				normalized = append(normalized, ch)
			}
		}
		out["choices"] = normalized
	}

	return out
}

func sanitizeContent(s string) string {
	return s
}

func normalizeMessage(msg map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range msg {
		if k == "provider_metadata" || k == "proxy_metadata" {
			continue
		}
		out[k] = v
	}
	if tc, ok := out["tool_calls"].([]any); ok && len(tc) > 0 {
		if out["content"] == nil {
			out["content"] = ""
		}
	}
	if c, ok := out["content"].(string); ok {
		out["content"] = sanitizeContent(c)
	}
	return out
}

func getNested(obj map[string]any, keys ...any) any {
	current := any(obj)
	for _, key := range keys {
		switch k := key.(type) {
		case string:
			if m, ok := current.(map[string]any); ok {
				current = m[k]
			} else {
				return nil
			}
		case int:
			if arr, ok := current.([]any); ok && k < len(arr) {
				current = arr[k]
			} else {
				return nil
			}
		default:
			return nil
		}
	}
	return current
}

// parseInferenceCapDuration 从 Cline 429 错误体中解析 "Try again in 17h 59m" 形式的等待时长。
// 支持 "17h 59m"、"17h"、"59m"、"30s"、"1d 2h 30m" 等组合。
func parseInferenceCapDuration(body string) time.Duration {
	// 在错误体中查找 "Try again in ..." 子串
	idx := strings.Index(body, "Try again in")
	if idx < 0 {
		return 0
	}
	rest := body[idx+len("Try again in"):]
	// 截取到下一个引号或换行
	end := len(rest)
	if i := strings.IndexAny(rest, "\"\n\r}"); i >= 0 {
		end = i
	}
	segment := strings.TrimSpace(rest[:end])
	return parseHumanDuration(segment)
}

// parseHumanDuration 解析 "17h 59m" / "2h" / "59m" / "30s" / "1d 2h" 之类的时长。
func parseHumanDuration(s string) time.Duration {
	if s == "" {
		return 0
	}
	var total time.Duration
	num := 0
	valid := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			num = num*10 + int(c-'0')
			valid = true
		case c == 'd':
			total += time.Duration(num) * 24 * time.Hour
			num, valid = 0, false
		case c == 'h':
			total += time.Duration(num) * time.Hour
			num, valid = 0, false
		case c == 'm' && i+1 < len(s) && s[i+1] == 's':
			total += time.Duration(num) * time.Millisecond
			num, valid = 0, false
			i++
		case c == 'm':
			total += time.Duration(num) * time.Minute
			num, valid = 0, false
		case c == 's':
			total += time.Duration(num) * time.Second
			num, valid = 0, false
		case c == ' ':
			// 分隔符
		default:
			// 未知字符，重置
			num, valid = 0, false
		}
	}
	if total <= 0 {
		return 0
	}
	_ = valid
	return total
}

// parseRetryAfter 解析 HTTP Retry-After 头（秒数或 HTTP 日期）。
func parseRetryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	// 尝试秒数
	if secs, err := parseIntSafe(header); err == nil {
		return time.Duration(secs) * time.Second
	}
	// 尝试 HTTP 日期
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}

func parseIntSafe(s string) (int, error) {
	var n int
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}
