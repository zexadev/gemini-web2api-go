# AGENTS.md: Developer & Agent Usage Guide for `gemini-web2api-go`

This guide specifies how autonomous agents, agentic harnesses, and client applications must interact with this local Gemini reverse-proxy bridge.

---

## 1. Service Overview & Endpoints

This service bridges standard OpenAI-compatible REST API calls to Google's browser-based Gemini interface using authenticated session cookies.

* **Base URL**: `http://127.0.0.1:8083/v1`
* **Chat Completions**: `POST http://127.0.0.1:8083/v1/chat/completions`
* **System & Scheduler Logs**: `GET http://127.0.0.1:8083/admin/api/logs?tail=50&format=text` (or `format=json`)
* **Telemetry / Usage**: `GET http://127.0.0.1:8083/admin/api/usage`
* **Cookie Pool Health**: `GET http://127.0.0.1:8083/admin/api/cookies`
* **Admin UI (No-Auth Monitoring)**: `http://localhost:8083/admin/`
* **API Key Header**: `Authorization: Bearer sk-gemini-d92939d594336a7a27ce745cfe4027876a4c485fc374c572`

---

## 2. Model Selection & Reasoning Effort

The bridge supports three thinking tiers via OpenAI-compatible parameters or model names:

### 2.1 Recommended for Agents: Medium Effort (Balanced)
Avoids Google's 8-minute high-effort thinking ceiling on large prompts while still conducting full multi-step reasoning:

```json
{
  "model": "gemini-4-neon",
  "reasoning_effort": "medium",
  "messages": [
    {"role": "user", "content": "..."}
  ],
  "stream": true
}
```
*(Equivalent model name: `"model": "gemini-4-neon-thinking-medium"`)*
* Reasoning is conducted internally on Google servers (~10–25s) without streaming out massive reasoning tokens, returning a clean final response.

### 2.2 Low / Normal Effort (Fast Execution)
Answers immediately in 2–6s without extended reasoning:
* `"model": "gemini-4-neon"` (or `"reasoning_effort": "low"`)

### 2.3 High Effort (Deep Chain-of-Thought)
Produces explicit `reasoning_content`:
* `"model": "gemini-4-neon-thinking"` (or `"reasoning_effort": "high"`)
* **Caution**: Only use on short prompts (<2,500 tokens). On large contexts, Google's upstream reasoning worker can exceed its ~8-minute execution budget and abort with HTTP 502 `inner[31]`.

---

## 3. Strict Operational Guardrails (Enforced by Server)

Because this service interacts with Google via an authenticated personal web session, **strict anti-abuse rules are enforced**:

### 3.1 In-Memory Queue & Strict Concurrency = 1 (Zero Overlap)
* Google web sessions assume a single human interacting in one browser tab. Parallel requests immediately trip Google's risk engine (`identity.hfcr`) and revoke session cookies.
* **Server-Side Queue**: The bridge implements an in-memory OS-style FIFO request queue. If concurrent requests arrive from multiple parallel tasks or agents, the bridge serializes them and dispatches them one by one.
* **Keepalive Multiplexing (`stream: true`)**: When using streaming mode, queued requests receive an instant `HTTP 200` with periodic SSE comment heartbeats (`: status=queued pos=1 ...`) every 4s to prevent client read timeouts while waiting.
* **Automatic Pacing Cooldown**: After each query finishes, the server enforces a mandatory 20-second anti-ban cooldown before dispatching the next queued job.
* **Queue Full / Disabled**: If the queue depth limit (default 20) is exceeded or queue is disabled, the server returns `HTTP 429 ip_slot_full`.
* **Agent Rule**: Always prefer `"stream": true` to receive queue progress and keep TCP sockets active. If you receive `HTTP 429`, sleep 20–30s and retry.

### 3.2 Client Request Timeout $\ge 600\text{s}$ (10 minutes)
* `gemini-4-neon-thinking` typically takes **45 to 140 seconds** on moderate inputs, but on large contexts (~40,000 tokens / 150 KB) with extensive reasoning, it can take **4 to 8 minutes (240–480s)**.
* The bridge server timeout is set to `request_timeout_sec: 600`.
* **Agent Rule**: Set your client HTTP timeout to at least **`600s`** (e.g. `timeout=600` in Python/fetch, or `--max-time 600` in curl). If the client aborts early, it severs the upstream socket and wastes quota.

### 3.3 Mandatory Inter-Request Pacing ($\ge 20\text{s}$)
* Always allow at least **20 seconds of idle cooldown** between the completion of one query and the start of the next.

### 3.4 Context Preservation via Automatic Text Attachment (>20,000 bytes)
* **No Lossy Compaction Needed**: Prompts up to **160,000 bytes (~40,000 tokens)** can be sent with full details intact (complete diffs, multiple files, complete test logs).
* **Automatic Server-Side Conversion**:
  * Prompts $\le 20,000$ bytes (~5,000 tokens) are transmitted inline as standard JSON text.
  * Prompts $> 20,000$ bytes are automatically uploaded by the bridge to Google push storage as `message.txt` and attached to the query with a short instruction pointer. This completely circumvents Google's upstream inline RPC body limit (~130 KB silent truncation) and naturalizes the interaction (mimicking human file uploads).
* **Hard Ingestion Ceiling**: Gemini Web's document ingestion pipeline can read up to **~160,000 bytes (~40,000 tokens)** across attachments. Prompts exceeding 160,000 bytes are rejected with `HTTP 400 context_length_exceeded`.

---

## 4. Error Codes & Recovery Semantics

| HTTP Status | Error Type | Meaning | Agent Action |
|---|---|---|---|
| **`429`** | `ip_slot_full` | Another query is currently running, or hourly limit (30 RPH) reached. | Sleep 20–30s and retry sequentially. Do not retry in parallel. |
| **`502`** | `upstream_error` | The session cookie has expired, or Google demanded a CAPTCHA challenge. | Pause task and notify operator to paste a fresh cookie from Firefox into `POST /admin/api/cookies`. Or fail-open to commercial fallback. |
| **`400`** | `context_length_exceeded` | Prompt exceeded maximum readable context length (160,000 bytes / ~40,000 tokens). | Reduce input to under 160,000 bytes and retry. |

### 4.1 Autonomous Telemetry & Log Inspection via REST
Autonomous agents and diagnostic scripts can query live bridge runtime logs directly via HTTP:
* **Plain text for CLI / subprocesses**: `GET http://127.0.0.1:8083/admin/api/logs?format=text&tail=50`
* **Structured JSON for agents**: `GET http://127.0.0.1:8083/admin/api/logs?format=json&level=ERROR&tail=20`
* **Filter by subsystem**: `GET http://127.0.0.1:8083/admin/api/logs?source=scheduler` (or `cookie`, `rotate`, `context`)

---

## 5. Sample Integration (Python)

```python
import urllib.request
import json

payload = {
    "model": "gemini-4-neon-thinking",
    "messages": [{"role": "user", "content": "Analyze this plan: ..."}],
    "stream": False
}

req = urllib.request.Request(
    "http://127.0.0.1:8083/v1/chat/completions",
    data=json.dumps(payload).encode("utf-8"),
    headers={
        "Content-Type": "application/json",
        "Authorization": "Bearer sk-gemini-d92939d594336a7a27ce745cfe4027876a4c485fc374c572"
    },
    method="POST"
)

# Timeout must be at least 180 seconds for reasoning turns
with urllib.request.urlopen(req, timeout=180) as resp:
    data = json.loads(resp.read().decode("utf-8"))
    choice = data["choices"][0]["message"]
    print("Reasoning:", choice.get("reasoning_content"))
    print("Answer:", choice.get("content"))
```
