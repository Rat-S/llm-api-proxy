# LLM API Proxy

A lightweight, high-performance, rate-limiting, and header-spoofing reverse proxy designed for LLM APIs. 

If your LLM gateway enforces strict rate limits (e.g., `429 Too Many Requests`) or restricts API access to specific developer clients, this proxy lets you **queue incoming requests** in memory and **inject required spoofed headers** (such as specific `User-Agent` or custom authentication metadata) before forwarding them upstream.

---

## Features

- **Token Bucket Rate Limiting with Reservation:** Smooths out incoming request spikes. If you hit your rate limit, the proxy buffers and queues the request in memory rather than returning a `429` error.
- **Client Disconnection Support:** Automatically detects if a client aborts or times out while waiting in the queue. It cancels the queue slot and refunds the reserved token so it isn't wasted on upstream calls.
- **Dynamic Header Injection:** Inject any HTTP headers (such as `User-Agent` or custom API keys/metadata) dynamically using simple environment variables.
- **Zero Dependencies:** Written in standard Go, compiling down to a single self-contained binary.

---

## Configuration

Configure the proxy at runtime using the following environment variables:

| Variable | Description | Default |
| :--- | :--- | :--- |
| `PROXY_TARGET_URL` | **Required.** The upstream LLM API base URL to proxy requests to (e.g., `https://api.openai.com` or any custom gateway). | *None* |
| `PROXY_PORT` | The port the proxy server will listen on. | `8318` |
| `RATE_LIMIT_RPM` | Requests per minute to allow. | `20` |
| `RATE_LIMIT_BURST` | The maximum burst capacity (tokens) allowed before queuing kicks in. | `5` |
| `HEADER_<NAME>` | Injects an HTTP header named `NAME` with the specified value. Single underscores are replaced with hyphens (e.g., `HEADER_User_Agent` maps to `User-Agent`). | *None* |
| `INJECT_HEADERS_JSON` | A JSON-formatted string representing a key-value map of headers to inject (useful for complex headers). | *None* |

---

## How to Run

### Native Go
Build and run the proxy locally:
```bash
# Set configuration env variables and run
export PROXY_TARGET_URL="https://your-upstream-api.com"
export RATE_LIMIT_RPM=20
export RATE_LIMIT_BURST=5
export HEADER_User_Agent="my-custom-client/1.0"

go run main.go
```

### Docker / Podman
Build a minimal, multi-stage Docker container:
```bash
# Build the container
docker build -t llm-api-proxy .

# Run the container
docker run -d \
  -p 8318:8318 \
  -e PROXY_TARGET_URL="https://your-upstream-api.com" \
  -e RATE_LIMIT_RPM=20 \
  -e RATE_LIMIT_BURST=5 \
  -e HEADER_User_Agent="my-custom-client/1.0" \
  --name llm-proxy \
  llm-api-proxy
```

---

## Example: Bypassing Client Restrictions

Some API gateways restrict access to official developer command-line tools by matching on specific headers (like `User-Agent` or version keys). You can bypass these restrictions by running this proxy to inject the expected headers.

### 1. Run the Proxy
```bash
export PROXY_TARGET_URL="https://api.upstream-service.com"
export PROXY_PORT="8318"
export RATE_LIMIT_RPM=20
export RATE_LIMIT_BURST=5

# Inject official client spoofing headers
export HEADER_User_Agent="official-cli-client/1.0.0"
export HEADER_X_Client_Version="1.0.0"

go run main.go
```

### 2. Configure Your Client
Point your client tool's base URL to the local proxy:
```bash
export UPSTREAM_BASE_URL="http://localhost:8318"
export UPSTREAM_API_KEY="your-api-key"
cli-tool-run
```

---

## License

This project is open-source and available under the [GNU Affero General Public License v3.0 (AGPLv3)](LICENSE).
