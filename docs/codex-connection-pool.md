# Codex 出站 HTTP/2 连接复用测试

本分支基于 `codex/codex-model-catalog` 的 `e7656f39524f9547138b2eb278f8d4404363e36c`，保留此前模型目录兼容改动。连接复用默认关闭，需要通过进程环境变量启用。

## 行为

```text
首个请求：TCP → 配置的 SOCKS5 / HTTP CONNECT 代理 → uTLS → HTTP/2
后续请求：复用存活连接，在独立 HTTP/2 stream 中发送
```

仅调整 Codex 执行器发往 `https://chatgpt.com` 的传输层，包括流式和非流式 Responses 调用。入口 Nginx、其他渠道以及控制面的 OAuth 刷新/配额查询保持既有路径。普通 API 渠道本来就有自己的连接池。

- 沿用 `HelloChrome_Auto` uTLS、正常证书和 SNI 验证，并要求 ALPN 为 `h2`。
- 尊重凭据/分组最终生效的代理、显式直连及环境代理 `NO_PROXY`。SOCKS5 目标域名交给代理解析。HTTP CONNECT 代理也支持复用。
- 账号范围由凭据 ID、身份代次和账号标识组成；代理 URL（含认证配置）取摘要后参与分区。普通 access token 刷新不新建池。不会把代理密码、访问令牌或原始代理 URL 写入新增日志。
- 最多缓存 64 个分区，包括尚未结束响应的退役分区；目标主机和端口由 HTTP/2 库管理。HTTP/2 流上限由协议/库处理，不额外限制每账号并发数。
- 首次冷启动等待连接取得后才放行同池其他请求，避免同时重复建连；不会等待第一个响应结束。
- 无活跃请求的分区和连接在空闲 60 秒后回收。活跃但暂时没有 SSE 内容的流不会因此被关闭。
- 单次 TCP＋代理协商＋TLS 建连预算为 30 秒，与长响应的生命周期分开。请求取消及时返回，遗留后台拨号仍受该预算和池退役取消控制。
- 请求结束或取消只释放自身 stream。凭据退役后让在途响应结束再回收；应用关闭会拒绝新池请求并参与原有排空流程。切换代理后新请求按自己的冻结配置选新分区，旧分区在空闲后回收。
- 满池且所有分区都在使用，或请求在凭据退役前冻结时，发送前选择使用相同代理的单请求 transport；不扩大长期缓存、不偷偷直连、不在失败后重复发送。单请求 transport 也使用同一套代理/uTLS 实现，并在响应结束时关闭。
- 不新增业务重试器，不添加幂等头来诱导底层重试。断线不能保证正在生成的请求恢复；由既有请求错误/重试规则处理。

## 在另一台服务器构建测试

需安装 Git、Docker 和 Docker Compose。以下创建独立测试目录和独立 Compose 项目/数据卷，不引用当前生产数据库。

```bash
git clone --branch codex/codex-connection-pool --single-branch https://github.com/xuanhen2013/gpt-load.git gpt-load-codex-pool
cd gpt-load-codex-pool
cp .env.example .env
```

编辑 `.env`：

```dotenv
PORT=3002
CODEX_CONNECTION_REUSE_ENABLED=true
LOG_LEVEL=debug
```

`HOST` 默认仅绑定本机；可通过已有受保护的反代访问，或使用 SSH 转发连接测试后台。新实例有独立管理密钥，在初始化测试实例后配置测试账号和现有 SOCKS5 代理。

```bash
docker compose -p gpt-load-codex-pool -f docker-compose.yml -f docker-compose.codex-pool-test.yml up -d --build
docker compose -p gpt-load-codex-pool -f docker-compose.yml -f docker-compose.codex-pool-test.yml logs --tail=100 gpt-load
curl --fail http://127.0.0.1:3002/health
```

源码构建会生成管理网页和 Go 程序。测试 override 明确指定本地镜像 `gpt-load:codex-connection-pool-test`，避免误用上游官方镜像。此分支没有要求先从镜像仓库拉取新的发布镜像。

## 如何判断已复用

启用后启动日志有 `event=codex_pool.enabled`。使用同一账号、同一代理，在 60 秒空闲期限内发送多次请求，调试日志包含：

| 日志 event | 含义 |
| --- | --- |
| `codex_pool.connection_opened` | 实际新建了一条出站连接；`proxy_connect_ms` 为 TCP＋代理协商总耗时，`tls_ms` 为 TLS 握手耗时 |
| `codex_pool.connection_acquired` | 请求取得连接；`reused=true` 表示复用已有连接 |
| `codex_pool.connection_closed` | 出站连接已关闭 |
| `codex_pool.capacity_fallback` | 所有缓存分区都在使用，本次请求走单请求 transport |

日志只用于诊断，不增加下游 API 响应头。多账号、多代理、端口变化、达到 HTTP/2 并发流限制或上游关闭连接，都可能合理地新建连接。连接复用并不保证某一条 TCP 永久在线，也不会缩短模型生成本身的时间。

关闭对照：把 `.env` 改为 `CODEX_CONNECTION_REUSE_ENABLED=false`，重新运行同一条 `docker compose ... up -d` 命令，使容器按新环境变量重新创建。无需重建镜像。默认关闭时恢复 CPA 原逐请求传输路径。

建议同代理、同账号、同模型比较：新建连接数、首事件 p50/p95、流完成率，以及长时间运行后的内存/FD。测试结束后把 `LOG_LEVEL` 调回 `info`。

## 本地验证与待验证边界

已提供真实本地 TLS/HTTP2 上游和认证代理的回归测试，覆盖跨请求上下文复用、并发冷启动、流取消、目标端口隔离、代理/账号身份隔离、令牌刷新、容量降级、空闲/凭据退役、应用排空、遗留握手清理、HTTP CONNECT、环境代理解析、错误证书/ALPN、服务端优雅关闭后的重连，以及上游读取 POST 后断线不重放。

Codex 执行器的真实流式/非流式调用入口也通过本地代理测试，包含配额观测与传输注入。测试只使用虚构凭据与本地服务器，不访问真实 ChatGPT 服务。

```bash
# 仓库根目录：应用及按应用依赖版本解析的嵌入模块
go test -race -count=1 -timeout=15m . ./internal/... github.com/router-for-me/CLIProxyAPI/v7/gptload-embedded/embedded

# 嵌入模块独立依赖图
cd third_party/cpaembedded
go mod tidy -diff
go vet ./...
go test -race -count=1 -timeout=5m ./...
```

真实第三方代理和 ChatGPT 的兼容性、Linux 实机表现及持续负载收益由测试服务器进一步验收。没有修改或重启现有生产服务器。
