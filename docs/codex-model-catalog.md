# Codex 模型目录（rc.6 定制补丁）

## 接口契约

- `GET /v1/models` 保留原 OpenAI `object/data` 返回格式。
- OpenAI 模型入口出现 `client_version` 查询参数即选择 Codex `{"models":[...]}`，包括空值和无法识别的版本。Anthropic / Gemini 格式不受影响。
- 不新增未鉴权入口，沿用现有 AccessKey 有效性、费用额度与请求限流检查。
- 目录只检查 `OpenAIResponses / OperationResponsesCreate` 候选，并按 AccessKey 的协议、模型和分组权限共同过滤。
- 对外 slug 使用公开路由名，能力查找使用上游模型名。未知元数据、同名路由存在不同能力或未知上游时保守排除，不影响普通模型列表或实际调用。
- 完整保留指令模板、上下文、工具与推理配置；仅对公开别名调整 slug/display_name，并在权限过滤后将 Astra 的 visibility 调整为 list。不擅自将其他隐藏模型展示。
- 可解析的旧客户端遵守 `minimal_client_version`，低于 0.144.0 时过滤 max/ultra 推理等级并修正默认值。无法解析的版本不猜测能力上限。
- 空目录返回 `[]`，实际序列化结果受原模型列表大小上限约束。Codex 响应标记 `Cache-Control: private, no-store`。

## 元数据来源与更新

`internal/gateway/codex_catalog.json` 是以下文件的完整副本，编译进服务，不在模型列表请求中请求海外上游：

- 仓库：<https://github.com/openai/codex>
- 标签：`rust-v0.153.1`
- 提交：`985641272869835d01d025ed2a218fbbce35fa9f`
- 文件：`codex-rs/models-manager/models.json`
- 许可证：Apache-2.0，副本位于 `LICENSES/Codex-Apache-2.0.txt`。

分组、模型与 AccessKey 路由变更会反映到后续目录请求；新增官方模型或能力字段需要核对并更新此固定目录、测试及重新构建。此补丁不是自动同步整个官方目录的后台任务，也没有把 GPT-Load 的 `/v1/models` 改为 CPA HTTP 透传。

## 客户端边界

客户端需要启用其远程目录能力并请求带 `client_version` 的模型入口。设置了本地 `model_catalog_json` 的 Codex 会优先使用本地文件；服务端补丁不会自动删除该设置。本次不改动用户日常 Codex 配置或缓存。

## 部署边界

此补丁基于 `v2.0.0-rc.6`，无需更换 CPA 模块，也不进行数据库迁移。部署前保存 Compose、原镜像引用和一致性数据备份；保留 rc.6 原镜像用于回滚。官方镜像升级会替换自定义二进制，需要重新应用和验证本补丁。
