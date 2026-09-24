package app

import (
	"bufio"
	"context"
	"io"
	"sync"
	"time"
)

var (
	// SSE 长连接在没有上游 token 时仍定期发送注释，避免反向代理或客户端把连接判定为空闲。
	streamHeartbeatInterval = 15 * time.Second
	// 上游超过该时间没有任何一行数据，视为卡死并主动结束读取。
	// 心跳只保活客户端连接，不应让代理无限等待失去响应的上游。
	streamReadIdleTimeout = 120 * time.Second
)

type streamLineResult struct {
	line string
	err  error
}

// readStreamLines 将可能阻塞的 Response.Body 读取放到独立 goroutine，调用方可以
// 同时处理 heartbeat、客户端取消和上游空闲超时。Response.Body.Close 会解除网络读取。
func readStreamLines(body io.ReadCloser) (<-chan streamLineResult, func()) {
	results := make(chan streamLineResult, 1)
	stop := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		defer close(results)
		reader := bufio.NewReader(body)
		for {
			line, err := reader.ReadString('\n')
			select {
			case results <- streamLineResult{line: line, err: err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return results, func() { stopOnce.Do(func() { close(stop) }) }
}

func newStreamIdleTimer() *time.Timer {
	if streamReadIdleTimeout <= 0 {
		return nil
	}
	return time.NewTimer(streamReadIdleTimeout)
}

func resetStreamIdleTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(streamReadIdleTimeout)
}

func stopStreamIdleTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func streamContextDone(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return nil
	}
	return ctx.Done()
}
