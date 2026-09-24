# 火山方舟 Responses 续聊

火山通道的 Responses 请求直接转发到火山原生 API，不再转换成 Chat Completions。

- 通道：`Volcengine Ark`
- Base URL：`https://ark.cn-beijing.volces.com/api/v3`
- 密钥：火山方舟 API Key
- 模型：使用支持 Responses API 的火山模型，例如 `deepseek-v4-pro-ga-260813`。

客户端仍向 GPT-Load 的 `/v1/responses` 发送请求。第一轮使用 `store: true`（或保留火山默认值），下一轮把返回的 `id` 放到 `previous_response_id`。GPT-Load 保存响应所属账号的绑定，火山保存实际对话上下文。续聊必须使用同一个 GPT-Load 访问密钥。

支持普通 JSON 和 SSE 响应，以及已存储响应的查询、删除和输入项查询。自定义 Base URL 包含完整 API 前缀；例如 `/api/v3` 后直接追加 `/responses`，不会再追加 `/v1`。

显式 `store: false` 会原样传递，不会为本轮响应创建新的续聊绑定。这类请求应由客户端携带完整历史。已有的上一轮存储响应仍可作为本轮输入。

升级前通过无状态转换返回的 ID 没有可恢复的火山 Responses 上下文。升级后需要新建会话，或先携带完整历史重新发起一轮请求，再使用新返回的响应 ID 续聊。

火山通道没有声明 Responses WebSocket、取消、压缩或后台任务能力。其他协议及通用 OpenAI Compatible 通道保留原来的路由行为。

参考：[火山 Base URL 与鉴权](https://docs.volcengine.com/docs/ark/base-url-and-authentication?lang=zh)、[模型列表](https://docs.volcengine.com/docs/ark/model-list?lang=zh)。
