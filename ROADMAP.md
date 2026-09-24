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
- 账号 token 刷新按错误类型区分：仅上游返回 `invalid_grant` 才标记 `expired`；DNS／网络中断、EOF、5xx 等临时故障保留原状态，并对临时故障最多重试 2 次（退避 0.5s／1s），`invalid_grant` 不重试。
- 新增后台 token 恢复循环：每 10 分钟重试一次 `expired` 账号，恢复后自动转回 `active`，不再依赖手动点测试按钮。
- token 刷新成功不再无条件写回 `active`，避免抹掉 429 冷却状态；管理台测试按钮与代理 401 分支遇到临时刷新故障时保留原状态。
- token 刷新按账号加锁串行化，避免并发刷新争抢轮换后的 refresh token 而被误判为 `invalid_grant`。
- Responses 流式转换的 tool call 改为按上游 `index`／`call_id` 分桶累积，每个调用独立对应一个 `function_call` output item；修复并行工具调用的 arguments 被拼成 `{...}{...}`、Codex 报 `failed to parse function arguments: trailing characters` 而中断对话的问题。
- Responses 的 `output_index` 改为统一分配器发放，reasoning／message／function_call 不再复用 0 和 1，避免同轮多 item 时索引冲突。
- Responses 的 `response.completed` 回填真实 usage（input／output／total／cached／reasoning tokens），并在 `response.output` 中带上 reasoning 与 message／function_call item。
- 转换 Responses 请求时强制打开 `stream_options.include_usage`，主动向上游索取 token 统计；上游未提供 usage 时按入站请求与已产出内容估算兜底并在日志标记，修复客户端 `token_count` 恒为 0、上下文永不触发自动压缩的问题。
- 非流式 Responses 响应补齐 reasoning item 与 usage 字段结构，兼容 `reasoning_content`／`reasoning` 与 `completion_tokens_details.reasoning_tokens`。
- 非流式 Responses 在上游缺失 usage 时也按请求体、正文、reasoning 和工具参数估算，避免聚合路径重新回到 token=0。
- token 估算改为按字符类别计价（CJK 按 1 token、其余按 4 字符 ≈ 1 token），替代原先的 JSON 字节数 / 4，避免中文会话被严重低估。
- Cline 上游请求最多总计尝试 3 次：429（含 500 中嵌入的 `INFERENCE_CAP_ERROR`）按账号冷却并切换，5xx／网络故障短暂冷却后重试；最终向客户端保留 429／5xx／网关错误状态码。
- Cline 上游请求绑定客户端 `context.Context`，客户端断开后取消上游请求；普通 Chat、Responses 和 Anthropic 流统一增加 `X-Accel-Buffering: no` 与 15 秒 SSE 心跳。
- 流式响应区分正常 `[DONE]` 和异常 EOF／上游错误／读空闲超时；Responses 异常结束发送 `response.failed`，不再伪造 `response.completed`。

## 进行中

- 待确认公网入口是否使用 Cloudflare。

## 待办

- 公网部署时配置 HTTPS 反向代理，并限制 `/admin/` 访问来源。
- 确认公网 API Key 强制校验和访问限流策略。
- `responsesToChat` 目前丢弃 client 的 `parallel_tool_calls` 等字段，评估是否需要透传。

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
- `docker-compose.yml` 默认注入 `ADMIN_PASSWORD`，本地使用前可直接修改密码。
- 2026-09-02 11:11（+0800）使用修复版隔离实例逐个真实调用管理接口测试 9 个账号：外层 HTTP 均为 200，但上游 HTTP 均为 429；JSON 均为 `success=false`、`data.status=cooldown`、`probeValid=false`，持久化状态均保持为 `cooldown`，未发生误恢复。
- 2026-09-02 12:02（+0800）再次逐个真实调用 9 个账号：账号 4、5 上游 HTTP 200 且收到有效回复，`success=true`、`status=active`、`probeValid=true` 并持久化为 active；其余 7 个上游 HTTP 429，均返回 `success=false`、`status=cooldown`、`probeValid=false`，接口语义和状态转换核验全部通过。
- 2026-09-02 17:56（+0800）账号次数与请求日志耗时回归验证：`go test ./...`、`go test -race ./...`、构建和 `git diff --check` 通过；次数持久化/跨日重置及日志中间件耗时记录测试通过。
- 2026-09-22 本地实测：使用已配置 API Key 调用 `z-ai/glm-5.3-flash` 的 `/v1/chat/completions`，`max_tokens=256` 返回 HTTP 200、`finish_reason=stop` 和正文 `4`；`max_tokens=16` 仅返回思考内容并因长度限制结束，非中转失败。
- 2026-09-23 账号过期误判回归：临时故障不改变账号状态且按 1+2 次请求重试；`invalid_grant` 立即标记 expired 且不重试；刷新成功保留 cooldown；恢复循环能把 expired 账号转回 active；管理台测试按钮在临时故障时保留原状态。`go test ./...`、`go test -race ./...`、`go vet ./...`、`go build -o /tmp/cline-proxy-verify .` 和 `git diff --check` 均通过。真实上游网络故障下的端到端恢复未验证。
- 2026-09-24 Responses tool call 与 usage 修复：新增单元测试覆盖并行调用拆分、参数跨 delta 累积、无 `index` 时按 `call_id` 分桶、晚到 `call_id` 绑定、残缺调用跳过、唯一 `output_index`、非流式/流式 usage 透传与缺失时估算；其中一条回归样例直接取自 2026-09-23 真实会话中被拼坏的 `exec_command` 参数。`go test ./...`、`go test -race ./...`、`go vet ./...`、`go build`、`git diff --check` 均通过。
- 2026-09-24 真实上游端到端验证（隔离实例 + 独立数据目录，未影响现有部署）：`/v1/responses` 流式请求返回上游真实 usage（input 33／output 55／reasoning 52），日志无估算标记；真实并行工具调用返回两个独立合法 JSON 的 `function_call`。Codex CLI 0.153.4 端到端：普通对话上报 `tokens used 6,879`（`input=6841 output=38`，修复前恒为 0），并行 `pwd`／`date` 场景两次调用同时执行（789ms／796ms），会话记录零参数解析错误。
- 2026-09-24 P2／P3 回归验证：账号额度错误切换、网络故障总计 3 次重试、最终错误状态码保留、Responses 异常 EOF／15 秒心跳（测试缩短为 5ms）均有测试覆盖；`go test ./...`、`go test -race ./...`、`go vet ./...`、构建和 `git diff --check` 均通过。
