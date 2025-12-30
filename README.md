# llm-router

基于 Go + Gin 的轻量 OpenAI 代理，仅转发 `/v1/chat/completions`，支持流式与非流式请求，并在失败时按模型顺序重试。

## 功能
- 仅代理 `/v1/chat/completions`，其他路径返回 404。
- 支持 `stream=true` 的流式透传。
- 失败自动重试：请求模型优先，其次按 `FALLBACK_MODELS` 顺序依次尝试。
- 可配置默认超时、备用模型超时，以及不重试的状态码。
- 请求头与请求体透传（保留 `Authorization`/`apiKey` 等）。

## 本地运行
```bash
go mod tidy
PORT=8080 OPENAI_BASE_URL=https://api.openai.com go run .
```

示例请求：
```bash
curl -XPOST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-xxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"ping"}]}'
```

## 环境变量
- `PORT`：监听端口，默认 `8080`。
- `OPENAI_BASE_URL`：上游 OpenAI 兼容地址，默认 `https://api.openai.com`。
- `DEFAULT_TIMEOUT`：主模型默认超时（非流式），默认 `60s`。
- `FALLBACK_MODELS`：备用模型列表，逗号分隔，按顺序重试。
- `FALLBACK_TIMEOUTS`：备用模型超时映射，格式 `model=15s,model2=20s`。
- `FALLBACK_DEFAULT_TIMEOUT`：备用模型默认超时（未命中映射时使用）；未设置则回退到 `DEFAULT_TIMEOUT`。
- `NO_RETRY_STATUS_CODES`：不重试的状态码列表，逗号分隔，默认 `400`。

说明：时间格式支持 `30s`、`2m` 等 Go duration，也支持纯数字秒数（如 `45`）。

## 重试规则
- 只要失败就重试，顺序为：请求模型 -> `FALLBACK_MODELS`。
- 若上游返回状态码命中 `NO_RETRY_STATUS_CODES`，直接返回，不再重试。
- 流式请求一旦开始向客户端写出数据，就不会再切换模型重试。

## Docker
```bash
docker build -t llm-router .
docker run -d --name llm-router -p 8080:8080 \
  -e OPENAI_BASE_URL=https://api.openai.com \
  -e DEFAULT_TIMEOUT=60s \
  -e FALLBACK_MODELS="gpt-4o-mini,gpt-4o" \
  -e FALLBACK_TIMEOUTS="gpt-4o-mini=15s,gpt-4o=20s" \
  -e NO_RETRY_STATUS_CODES="400,401" \
  llm-router
```
