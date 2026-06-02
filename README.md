# API Proxy

A lightweight, high-performance reverse proxy for LLM APIs (like Google Gemini). It provides a fully **OpenAI-compatible** interface, allowing you to seamlessly integrate various backend models into tools that expect the standard OpenAI API structure.

## Features

- **OpenAI Compatible Interface**: Exposes `/v1/chat/completions` and `/v1/models` endpoints.
- **Model-Based Routing**: Dynamically routes requests based on the `{"model": "..."}` JSON payload.
- **Model Rewriting**: Automatically translates the client-requested model name into the backend provider's exact model name (e.g., mapping `gemini-flash-lite` to `models/gemini-2.5-flash-lite`).
- **Load Balancing**: Distributes requests across multiple API keys in a round-robin fashion.
- **Smart Rate Limiting**: 
  - Tracks rate limits per model (independent buckets) or globally per provider.
  - Temporarily cools down keys when they hit `429 Too Many Requests`.
- **Streaming Support**: Fully supports HTTP Server-Sent Events (SSE) streaming without buffering.
- **Proxy Support**: Connect to upstream APIs via HTTP, HTTPS, or SOCKS5 proxies.

## Configuration

The proxy is configured via a YAML file (`config.yaml`).

### Example `config.yaml`

```yaml
listen: "0.0.0.0:3000"
max_body_size: 10485760 # 10MB
base_path: "/v1"

# Client authentication keys (checked against incoming requests)
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

## Running the Proxy

1. Clone the repository
2. Create your `config.yaml`
3. Run the proxy:
   ```sh
   go run . -config config.yaml
   ```

To build a standalone executable:
```sh
go build -o api-proxy .
```

## Usage

Point your OpenAI-compatible client library (e.g., Python `openai` package) to the proxy:

```bash
curl -X POST http://localhost:3000/v1/chat/completions \
  -H "Authorization: Bearer my-client-secret-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-flash-lite",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

You can also retrieve the list of configured models:
```bash
curl http://localhost:3000/v1/models \
  -H "Authorization: Bearer my-client-secret-key"
```
