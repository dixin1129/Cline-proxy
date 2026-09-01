package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"cline-go-proxy/internal/kit"
)

// RequestLog 单条代理请求记录（对话/API 调用历史）
type RequestLog struct {
	Time     time.Time `json:"time"`
	Client   string    `json:"client"`
	Method   string    `json:"method"`
	Path     string    `json:"path"`
	Model    string    `json:"model,omitempty"`
	Account  string    `json:"account,omitempty"`
	Route    string    `json:"route"` // zen | cline | admin | other
	Status   int       `json:"status"`
	Duration int64     `json:"duration_ms"`
	Input    int64     `json:"inputTokens,omitempty"`
	Output   int64     `json:"outputTokens,omitempty"`
	Note     string    `json:"note,omitempty"`
}

const (
	maxReqLogs     = 500
	maxReqLogsFile = 10 << 20 // 10MB 上限，超出后清空落盘文件（内存仍保留最近 500 条）
)

var (
	reqLogsMu sync.Mutex
	reqLogs   []RequestLog
)

var reqLogsFile = kit.ResolveDataPath("requests.jsonl")

type apiKeyAuthMeta struct {
	authed bool
}

type requestLogMeta struct {
	account string
	input   int64
	output  int64
}

type requestLogMetaKey struct{}

func withRequestLogMeta(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), requestLogMetaKey{}, &requestLogMeta{}))
}

func requestLogMetaFrom(r *http.Request) *requestLogMeta {
	if meta, ok := r.Context().Value(requestLogMetaKey{}).(*requestLogMeta); ok {
		return meta
	}
	return &requestLogMeta{}
}

func setRequestLogMeta(r *http.Request, account string, input, output int64) {
	if meta, ok := r.Context().Value(requestLogMetaKey{}).(*requestLogMeta); ok {
		meta.account = account
		meta.input = input
		meta.output = output
	}
}

type apiKeyAuthMetaKey struct{}

// AppendReqLog 记录一条请求日志：内存环形保留 + 异步追加落盘
func AppendReqLog(l RequestLog) {
	reqLogsMu.Lock()
	reqLogs = append(reqLogs, l)
	if len(reqLogs) > maxReqLogs {
		reqLogs = reqLogs[len(reqLogs)-maxReqLogs:]
	}
	reqLogsMu.Unlock()
	go func() {
		data, _ := json.Marshal(l)
		f, err := os.OpenFile(reqLogsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return
		}
		f.Write(append(data, '\n'))
		f.Close()
		if st, err := os.Stat(reqLogsFile); err == nil && st.Size() > maxReqLogsFile {
			os.WriteFile(reqLogsFile, nil, 0600)
		}
	}()
}

// LoadRequestLogs 返回最近的请求日志（内存优先，启动后从落盘文件补载）
func LoadRequestLogs() []RequestLog {
	reqLogsMu.Lock()
	defer reqLogsMu.Unlock()
	logs := make([]RequestLog, 0, len(reqLogs))
	for _, l := range reqLogs {
		if l.Route == "cline" {
			logs = append(logs, l)
		}
	}
	return logs
}

// LoadRequestLogsFromFile 启动时从落盘文件读取尾部记录
func LoadRequestLogsFromFile() {
	raw, err := os.ReadFile(reqLogsFile)
	if err != nil {
		return
	}
	lines := splitLinesSafe(string(raw))
	for _, line := range lines {
		if line == "" {
			continue
		}
		var l RequestLog
		if json.Unmarshal([]byte(line), &l) == nil {
			if l.Route != "cline" {
				continue
			}
			reqLogs = append(reqLogs, l)
		}
	}
	if len(reqLogs) > maxReqLogs {
		reqLogs = reqLogs[len(reqLogs)-maxReqLogs:]
	}
}

func splitLinesSafe(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// ============ 请求日志中间件 ============

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// requestLogMiddleware 提取请求模型并记录 cline 池 API 调用。
func requestLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		meta := &apiKeyAuthMeta{}
		r = withRequestLogMeta(r)
		r = r.WithContext(context.WithValue(r.Context(), apiKeyAuthMetaKey{}, meta))

		model := ""
		route := "cline"
		bodyBytes, _ := io.ReadAll(r.Body)
		if len(bodyBytes) > 0 {
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			var probe struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(bodyBytes, &probe) == nil {
				model = probe.Model
			}
		}

		next.ServeHTTP(sw, r)

		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		if model == "" {
			return
		}
		if !meta.authed || routeModel(model) != "cline" {
			return
		}
		client := r.RemoteAddr
		if host, _, err := net.SplitHostPort(client); err == nil {
			client = host
		}
		logMeta := requestLogMetaFrom(r)
		AppendReqLog(RequestLog{
			Time:     time.Now(),
			Client:   client,
			Method:   r.Method,
			Path:     r.URL.Path,
			Model:    model,
			Account:  logMeta.account,
			Route:    route,
			Status:   sw.status,
			Duration: time.Since(start).Milliseconds(),
			Input:    logMeta.input,
			Output:   logMeta.output,
		})
	})
}
