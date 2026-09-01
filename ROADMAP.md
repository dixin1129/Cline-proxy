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
- Responses API 将 `reasoning.effort` 映射为上游 `reasoning_effort`，并针对 GLM 默认启用思考。
- Responses/chat 转换兼容上游 `reasoning` 字段并强制 GLM `enable_thinking`；补齐 Codex 要求的 reasoning item `summary` 字段、动态 output index 与 completed usage 结构；转发请求显式 Accept SSE；日志中间件透传 Flush，修复流式响应被缓冲或 Codex 丢弃导致无思考/超时的问题。

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
