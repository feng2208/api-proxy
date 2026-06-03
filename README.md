# API 代理 (API Proxy)

一个轻量级、高性能的 LLM API（如 Google Gemini）反向代理服务器。它提供完全**兼容 OpenAI**的接口，使您能够无缝地将各种后端模型集成到期望标准 OpenAI API 结构的工具中。

## 功能特性

- **兼容 OpenAI 接口**：暴露 `/v1/chat/completions` 和 `/v1/models` 端点。
- **基于模型的路由**：根据 `{"model": "..."}` JSON 请求体动态路由请求。
- **模型重写**：自动将客户端请求的模型名称翻译为后端提供商的精确模型名称（例如，将 `gemini-flash-lite` 映射为 `models/gemini-2.5-flash-lite`）。
- **负载均衡**：以轮询（Round-Robin）方式在多个 API Key 之间分配请求。
- **智能速率限制**：
  - 针对每个模型或每个提供商全局跟踪速率限制。
  - 当 Key 触发 `429 Too Many Requests` 时，临时冷却该 Key。
- **流式传输支持**：完全支持 HTTP 服务器发送事件（SSE）流式传输，无缓冲。
- **代理支持**：通过 HTTP 或 SOCKS5 代理连接到上游 API。

## 配置说明

代理通过 YAML 文件（`config.yaml`）进行配置。

### `config.yaml` 示例

```yaml
listen: "0.0.0.0:3000"
max_body_size: 10485760 # 10MB
base_path: "/v1"

# 客户端认证密钥
client_rate_limit: 10
api_keys:
  - my-client-secret-key

models:
  - name: gemini-flash-lite
    providers:
      - name: gemini
        upstream: https://generativelanguage.googleapis.com/v1beta/openai/v1
        model: models/gemini-2.5-flash-lite
        model_rate_limit: 10
        timeout: 30s

auth:
  providers:
    - name: gemini
      rate_limit: 15
      keys:
        - YOUR_GEMINI_API_KEY_1
        - YOUR_GEMINI_API_KEY_2
```

## 使用方法

将 OpenAI 兼容客户端库（例如 Python 的 `openai` 包）指向该代理：

```bash
curl -X POST http://localhost:3000/v1/chat/completions \
  -H "Authorization: Bearer my-client-secret-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-flash-lite",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

获取已配置的模型列表：
```bash
curl http://localhost:3000/v1/models \
  -H "Authorization: Bearer my-client-secret-key"
```
