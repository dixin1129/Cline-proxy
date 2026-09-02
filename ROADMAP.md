# Roadmap

## 当前阶段

可用的本地代理工具，正在补齐公网部署安全能力。

## 已完成

- 管理后台 `/admin/` 与 `/admin/api/*` 增加 Basic Auth。
- 管理密码支持 `ADMIN_PASSWORD` 环境变量和 `-admin-password` 参数。
- 未配置管理密码时关闭 `/admin/`，返回 503。
- 移除启动时自动结束目标端口进程的 `freePort` 逻辑，端口被占用时启动失败。
- 请求日志只记录通过 API Key 鉴权且路由到 cline 账号池的请求。
- 请求日志增加账号邮箱、输入 token 数和输出 token 数，管理后台同步展示。
- 管理台账号列表同时展示本地今日/累计调用次数与 Token 消耗，次数用于观察按调用次数限制的渠道使用量。
- 修复管理台请求日志耗时字段读取错误，正确展示后端记录的 `duration_ms`，并兼容旧的 `durationMs` 字段。
- Responses API 将 `reasoning.effort` 映射为上游 `reasoning_effort`，并针对 GLM 默认启用思考。
- Responses/chat 转换兼容上游 `reasoning` 字段并强制 GLM `enable_thinking`；补齐 Codex 要求的 reasoning item `summary` 字段、动态 output index 与 completed usage 结构；转发请求显式 Accept SSE；日志中间件透传 Flush，修复流式响应被缓冲或 Codex 丢弃导致无思考/超时的问题。
- 管理台账号测试不再仅凭 HTTP 200 判定可用：探测请求会校验 JSON／SSE 中的有效 assistant 回复，识别 HTTP 200 内嵌限流错误，并保留失败前的账号状态。

## 进行中

- 待确认公网入口是否使用 Cloudflare。

## 待办

- 公网部署时配置 HTTPS 反向代理，并限制 `/admin/` 访问来源。
- 确认公网 API Key 强制校验和访问限流策略。

## 最近验证

- `go test ./...` 通过。
- `go build -o /tmp/cline-proxy-build .` 通过。
- 本地验证：Cline 三类代理入口均可回传账号与 usage，`git diff --check` 通过。
- 本地验证：请求日志不再包含管理台、元信息、zen 及未通过 API Key 鉴权的请求。
- 本地验证：未配置密码返回 503；错误密码返回 401；正确 Basic Auth 可访问 `/admin/`；`/health` 不受影响。
- `go test ./...`、`go build -o /tmp/cline-proxy-think2 .` 和 `git diff --check` 通过。
- 本地实测：`/v1/responses` 流式返回 reasoning 与 output_text；GLM 思考内容为 `17 × 24 = 408.`，正文为 `17 乘 24 的结果是 **408**。`。
- 本地实测：代理 chat 流 5 次连续返回 reasoning，`go test ./...`、`go build -o cline-proxy .` 和 `git diff --check` 通过。
- Codex CLI 0.144.1 端到端验证：修复前持续报告 `ReasoningSummaryDelta without active item`；修复后复杂请求正常产出 reasoning 与 agent message，且不再出现该错误。
- 管理台探测回归测试：HTTP 200 但只有 reasoning、空响应、SSE 错误均不会恢复账号；有效文本／tool call 才会标记可用；`go test ./...`、`go test -race ./...`、构建和 `git diff --check` 通过。
- 2026-09-02 11:11（+0800）使用修复版隔离实例逐个真实调用管理接口测试 9 个账号：外层 HTTP 均为 200，但上游 HTTP 均为 429；JSON 均为 `success=false`、`data.status=cooldown`、`probeValid=false`，持久化状态均保持为 `cooldown`，未发生误恢复。
- 2026-09-02 12:02（+0800）再次逐个真实调用 9 个账号：账号 4、5 上游 HTTP 200 且收到有效回复，`success=true`、`status=active`、`probeValid=true` 并持久化为 active；其余 7 个上游 HTTP 429，均返回 `success=false`、`status=cooldown`、`probeValid=false`，接口语义和状态转换核验全部通过。
- 2026-09-02 17:56（+0800）账号次数与请求日志耗时回归验证：`go test ./...`、`go test -race ./...`、构建和 `git diff --check` 通过；次数持久化/跨日重置及日志中间件耗时记录测试通过。
