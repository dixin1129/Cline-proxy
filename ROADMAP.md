# Roadmap

## 当前阶段

可用的本地代理工具，正在补齐公网部署安全能力。

## 已完成

- 管理后台 `/admin/` 与 `/admin/api/*` 增加 Basic Auth。
- 管理密码支持 `ADMIN_PASSWORD` 环境变量和 `-admin-password` 参数。
- 未配置管理密码时关闭 `/admin/`，返回 503。
- 移除启动时自动结束目标端口进程的 `freePort` 逻辑，端口被占用时启动失败。
- 请求日志只记录通过 API Key 鉴权且路由到 cline 账号池的请求。

## 进行中

- 待确认公网入口是否使用 Cloudflare。

## 待办

- 公网部署时配置 HTTPS 反向代理，并限制 `/admin/` 访问来源。
- 确认公网 API Key 强制校验和访问限流策略。

## 最近验证

- `go test ./...` 通过。
- 本地验证：请求日志不再包含管理台、元信息、zen 及未通过 API Key 鉴权的请求。
- 本地验证：未配置密码返回 503；错误密码返回 401；正确 Basic Auth 可访问 `/admin/`；`/health` 不受影响。
