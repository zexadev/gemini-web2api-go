# gemini-web2api-go

<img src="docs/banner.svg" alt="gemini-web2api-go" width="100%">

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.21%2B-00ADD8.svg)](https://golang.org)
[![Docker](https://img.shields.io/badge/docker-distroless-blue)](Dockerfile)

[中文文档](README.md) | English

Turn the Google Gemini web app into an OpenAI-compatible API. **Single binary**, **no account needed** (anonymous works), **real Chrome 146 fingerprint**, **SQLite persistence**, ships with an **admin dashboard**.

---

## What this is

It turns a call like this:
```
[OpenAI SDK / Cherry Studio / Cursor / dify / newapi / ...]
    ↓ http://localhost:8083/v1/chat/completions
[gemini-web2api-go]
    ↓ reverse-engineered gemini.google.com web protocol
[Google Gemini web app]
```

This is not a wrapper around Google's official API ([generativelanguage.googleapis.com](https://generativelanguage.googleapis.com)) — it proxies **the browser protocol directly**, so **no Google API key and no paid quota are required**.

## Features

**API**
- OpenAI-compatible: `/v1/chat/completions`, `/v1/models`, `/v1/responses`
- Real incremental streaming for plain chat (requests with `tools`, and `/v1/responses`, are buffered then sent)
- Bearer token / `x-api-key` auth; the key can be rotated from the panel
- `usage` computed with tiktoken; `reasoning_tokens` counted separately, not folded
  into `completion_tokens`

**Models**
- `gemini-3.6-flash` and `gemini-3.5-flash-lite` work anonymously, web search included
- `gemini-3.1-pro` needs a cookie; every reply carries a reasoning chain
- Each of the three models has a `-thinking` variant (extended thinking), available with a cookie
  (`reasoning_content`)
- Every response records which model the backend **actually** used, so silent
  downgrades are visible

**Staying unblocked, and scaling past one IP**
- Real Chrome 146 TLS fingerprint via utls, not a default SDK handshake
- Per-exit-IP limits: concurrency / RPM / RPH
- Proxy pool: runtime CRUD, failure circuit breaker, rotation — each proxy is its
  own rate-limit slot
- Cookie pool: several Google accounts rotated least-recently-used first, auto-refreshed and kept alive, each account pinned to its own exit

**Operations**
- Single binary, cross-compiled for 6 platforms; container image built on distroless
- SQLite persistence: 30 days of per-request detail plus permanent aggregates
- Admin panel: overview / requests / proxy pool / cookie pool / settings, with
  config changes applied without a restart
- **Prompt and response bodies are never stored** — metadata only (length, latency,
  model, status)

## Quick start

### Prebuilt binary (simplest)

Grab the one for your platform from
[Releases](https://github.com/zexadev/gemini-web2api-go/releases). No Go, no Docker —
the single file is the whole thing:

```bash
chmod +x gemini-web2api-go_*
./gemini-web2api-go_* --port 8083 --admin-token your-admin-token
```

Data lands in `./data/gemini.db` by default; pass `--db /your/path.db` to move it.

### Docker (no source needed)

```bash
docker run -d --name gemini-web2api \
  -p 127.0.0.1:8083:8083 \
  -v "$PWD/data:/data" \
  -e ADMIN_TOKEN=your-admin-token \
  ghcr.io/zexadev/gemini-web2api-go:latest
```

For compose, just grab the file — no clone required:

```bash
curl -O https://raw.githubusercontent.com/zexadev/gemini-web2api-go/main/docker-compose.yml
ADMIN_TOKEN=your-admin-token docker compose up -d
```

### From source

If you have Go, skip Docker entirely:

```bash
git clone https://github.com/zexadev/gemini-web2api-go
cd gemini-web2api-go
go build -o gemini-web2api-go .
./gemini-web2api-go --port 8083 --admin-token your-admin-token
```

Changed the code and want it in a container? Uncomment the two `build:` lines in
`docker-compose.yml`, then `docker compose up -d --build`.

The startup banner:

```
gemini-web2api-go v4.0.0
  Listening:   http://0.0.0.0:8083
  Base URL:    http://localhost:8083/v1
  API key:     sk-gemini-XX...XXXX  (mutable in admin UI)
  Admin UI:    http://localhost:8083/admin  (token auth)
  DB:          ./data/gemini.db
  Models:      [gemini-3.5-flash-lite gemini-3.6-flash]
  Cookie:      none (anonymous)
  Proxy:       none
  Impersonate: chrome_146
  Tokenizer:   tiktoken cl100k_base
  Per-IP 限流: 并发=5 / RPM=30 / RPH=80
  Retry:       3x / 2s
```

An API key is generated on first boot. The banner masks it; the full value is on the **Settings** page of the admin panel.

### Making a call

```bash
curl http://localhost:8083/v1/chat/completions \
  -H "Authorization: Bearer sk-gemini-..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.6-flash",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

The OpenAI Python SDK works as-is:

```python
from openai import OpenAI
client = OpenAI(
    base_url="http://localhost:8083/v1",
    api_key="sk-gemini-..."  # find it in the admin panel
)
resp = client.chat.completions.create(
    model="gemini-3.6-flash",
    messages=[{"role": "user", "content": "Explain quantum entanglement"}]
)
print(resp.choices[0].message.content)
```

On Windows PowerShell, `curl` is an alias for `Invoke-WebRequest` and will reinterpret the JSON quoting, so use `curl.exe` with `--%`:

```powershell
curl.exe --% http://127.0.0.1:8083/v1/chat/completions -H "Content-Type: application/json" -H "Authorization: Bearer sk-gemini-..." -d "{\"model\":\"gemini-3.6-flash\",\"messages\":[{\"role\":\"user\",\"content\":\"Hello!\"}]}"
```

## Connecting clients

Every OpenAI-compatible client (Cherry Studio / ChatBox / Open WebUI / dify / Cursor / …) is configured the same way:

| Field | Value |
|---|---|
| Base URL | `http://localhost:8083/v1` (some clients want just `http://localhost:8083` and append `/v1` themselves) |
| API key | the `sk-gemini-…` shown on the admin panel's Settings page |
| Model | `gemini-3.6-flash` |

**newapi / one-api channel**: pick the OpenAI type, set the base URL to `http://localhost:8083` (use `http://host.docker.internal:8083` or the container name if newapi runs in Docker too), paste the API key, and list the models as `gemini-3.6-flash,gemini-3.5-flash-lite`.

**Codex CLI** uses `/v1/responses` — point its base URL at `http://localhost:8083/v1`. The endpoint is implemented, but it is not incrementally streamed (see the protocol coverage table).

**Gemini CLI cannot connect.** It expects Google's native `/v1beta/models/{model}:generateContent`; this project only exposes OpenAI-shaped endpoints and does not implement `/v1beta`.

There is also an unauthenticated health check at `GET /` returning `{"status":"ok","version":…,"models":[…]}`.

## MCP (web_search tool)

Besides the OpenAI API, the same process and port also serves an **MCP server** at `/mcp`,
exposing Gemini's web-grounded **search** as a `web_search` tool. This lets MCP clients
(Claude Desktop / Claude Code / Cursor) "search the web through Gemini" and get a
**synthesized answer plus source links** back.

No separate process or deployment — running the backend gives you this. The transport is
HTTP (Streamable HTTP), so remote clients just point at the URL, reusing the backend's
account pool / proxy pool / rate limiting. Search works anonymously; no cookie required.

**Client config** (Claude Desktop's `claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "gemini-search": {
      "url": "http://your-server:8083/mcp",
      "headers": { "Authorization": "Bearer sk-gemini-your-key" }
    }
  }
}
```

- `url` points at the backend's `/mcp`; locally it's `http://localhost:8083/mcp`.
- `Authorization` is the same API key as the OpenAI endpoint (shown on the Settings page).
- Claude Code: `claude mcp add --transport http gemini-search http://localhost:8083/mcp --header "Authorization: Bearer sk-gemini-your-key"`.

The `web_search(query)` tool takes a query/question and returns Gemini's web-grounded
answer with a `Sources:` list appended. It's the only tool for now (`url_context` — reading
a specific URL's content — is not implemented yet).

## Admin panel

`http://localhost:8083/admin`, log in with `--admin-token`.

- **Overview** — 24h KPIs, request volume / P50 latency dual-axis chart, per-model and per-proxy breakdowns, IP rate-limit usage, one-click connectivity check
- **Requests** — request log (metadata only, no prompt/response content), status/model filters, pagination
- **Proxy pool** — runtime CRUD, enable/disable, circuit breaker on repeated failures (each proxy is its own IP slot)
- **Cookie pool** — import multiple signed-in Google accounts; requests rotate through them **least-recently-used first**. One-click "check" tells you whether an account is still signed in; cookies are auto-refreshed and kept alive every 10 minutes; each account is pinned to its own exit. The list shows redacted summaries only (cookie count / key entries / last 4 of SAPISID / failure count)
- **Settings** — runtime config form (saved settings take effect immediately), API key rotation, read-only view of deploy-time config

The frontend is a single HTML file and Chart.js is embedded in the binary, **not loaded from a CDN**, so the panel also works on air-gapped or intranet deployments.

**Serving it under a path prefix needs no extra configuration**: every URL in the panel is relative, so forwarding `https://example.com/gemini/` to this service's `/` is enough to open `https://example.com/gemini/admin`. (Requesting `/admin` without the trailing slash 301s to `admin/` — relative URLs resolve against the document URL's *directory*, and the two forms differ by one level, so they have to be normalised first.)

## Models

Gemini's backend only recognises three models (the list comes from `batchexecute?rpcids=otAQ7b`):

| Model | Description |
|---|---|
| `gemini-3.6-flash` | All-round, default |
| `gemini-3.5-flash-lite` | Fast and lightweight |
| `gemini-3.1-pro` | Most capable, **needs a cookie**; every reply carries a reasoning chain |
| `gemini-3.7-flash` | Newer Flash, **needs a cookie on an account already rolled out to 3.7** (otherwise downgraded to 3.5 Flash-Lite) |
| `gemini-3.6-flash-thinking` | 3.6 Flash with extended thinking; **needs a cookie** |
| `gemini-3.5-flash-lite-thinking` | 3.5 Flash-Lite with extended thinking; **needs a cookie** |
| `gemini-3.1-pro-thinking` | 3.1 Pro with extended thinking; **needs a cookie** |
| `gemini-3.7-flash-thinking` | 3.7 Flash with extended thinking; **needs a cookie on a 3.7-enabled account** |
| `gemini-image` | Image generation (Nano Banana); base64 output; **needs a cookie** |
| `gemini-music` | Music (Lyria, ~30s); base64 output; **needs a cookie** |
| `gemini-canvas` | Canvas: generates an interactive HTML document (returned inline as a ```html block); **needs a cookie** |
| `gemini-video` | Video generation (async, tens of seconds to a few minutes); base64 MP4 output; **needs a Pro/paid account** (free accounts are refused by the upstream content policy) |

Without a cookie, `/v1/models` returns only the first two, and asking for `gemini-3.1-pro` fails with an explanation. An anonymous request for it is always silently downgraded to 3.5 Flash-Lite — better to fail at model selection than to hand back a reply that "succeeded" but isn't Pro.

With a valid cookie it really is Pro: six consecutive calls all had the backend report `3.1 Pro` itself.

Only those three are exposed. The old names `gemini-3.5-flash`, `gemini-3.5-flash-thinking`, `gemini-3.5-flash-thinking-lite`, `gemini-auto` and `gemini-flash-lite` were **removed** (they now return 400): the backend has no entries for them, and keeping them only suggested there were five distinct models to choose from.

> **`@think=N` is deprecated.** The suffix was written into `inner[17]` and long treated as "thinking depth", but captures show it is the **turn index within a conversation** (first turn `[[0]]`, the follow-up carrying a conversation id `[[1]]`, incrementing from there) — nothing to do with reasoning depth. We open a fresh conversation for every request, so the value is always 0 and the parameter never did anything. The suffix is still accepted and ignored, so existing client configs don't break.

### Reasoning chain (`reasoning_content`)

`gemini-3.1-pro` emits its own reasoning before every answer. The project exposes it
as `reasoning_content`, the de-facto standard field, so clients like newapi, Cherry
Studio and ChatBox render it as a collapsible "thinking" panel. The other two models
produce no reasoning chain and the field is then omitted entirely.

Non-streaming:

```json
{"choices":[{"message":{
  "role":"assistant",
  "content":"The core rule of this classic puzzle is...",
  "reasoning_content":"**Defining the Constraints**\n\nI've successfully defined..."
}}],
 "usage":{"prompt_tokens":26,"completion_tokens":337,"reasoning_tokens":77,"total_tokens":363}}
```

Streaming sends it as `delta.reasoning_content`, and **the whole reasoning chain is
streamed before any answer text** — that is the order the upstream itself uses, so the
experience matches the Gemini web UI.

`reasoning_tokens` is counted **separately and is not included in `completion_tokens`**:
clients collapse the reasoning by default, so billing it would charge users for output
they never see (newapi bills on `completion_tokens`). Add the two together yourself if
you do want to bill for it.

### Known capability boundaries

Anonymous calls (no cookie) only reach the two text models above plus Gemini's built-in web search. `gemini-3.1-pro` is silently downgraded to 3.5 Flash-Lite anonymously, which is why it isn't exposed at all in that case.

Attaching a cookie additionally unlocks `gemini-3.1-pro`, **extended thinking for all three models**, **image / video input**, **a longer context** (over-long conversations are sent as a text attachment, see below), and **image generation (`gemini-image`), music (`gemini-music`), canvas (`gemini-canvas`), video generation (`gemini-video`, needs a Pro account)**.

Deep research is still **not implemented** (a multi-step async flow). The panel's "actual model" column always shows which model the backend really used, so any downgrade is visible.

### Image & music

`gemini-image` (Nano Banana) and `gemini-music` (Lyria, ~30s) go through `/v1/chat/completions` just like a normal chat — put the image/music description in the user message. The bytes come back as a **base64 data URL** inside `content`: images as `![image](data:image/png;base64,…)`, audio as `[audio](data:audio/mpeg;base64,…)`. Markdown-capable clients render the image inline; to save a file, decode the base64 after the comma.

```bash
curl http://127.0.0.1:8083/v1/chat/completions \
  -H "Authorization: Bearer sk-gemini-..." -H "Content-Type: application/json" \
  -d '{"model":"gemini-image","messages":[{"role":"user","content":"an orange cat in an astronaut helmet"}]}'
```

Not returning a URL is deliberate — Gemini's artifact links need the cookie to download, so a bare link would be dead on the client side; the server fetches the bytes and inlines them. The base64 is **not counted in `completion_tokens`** (a single image is millions of tokens; billing that downstream would be absurd), only the caption text is. Both need a cookie, so they don't appear in `/v1/models` without one. Params like `size` / `n` have no upstream knob and are ignored.

**Multi-turn context is implemented by flattening `messages` into a single prompt** (the web protocol's native multi-turn needs a token that only a browser JS runtime can produce, which plain HTTP cannot forge). The cost is that every turn resends the whole history, which runs into the single-request length wall: about **130,000 UTF-8 bytes**. Past that the upstream **silently truncates from the tail without an error** — and since the newest message sits at the end, what gets eaten is exactly what you just asked, which reads as "the model suddenly got dumb".

With a cookie, an over-long conversation is uploaded as a `message.txt` attachment instead, which gets around the request-body wall — **but the attachment has a wall of its own**: the model only ever sees about **160,000 bytes** of content in total; anything past that is uploaded but never read (measured with the total size held fixed and only the marker's offset moved: readable at 157,833, not readable at 163,371; splitting into several attachments does not raise the budget). So a cookie takes the usable length from 130K to about 160K — **a genuinely long conversation still has to be compacted by the client**. Without a cookie the request is rejected with 400 `context_length_exceeded` rather than losing data silently.

## Configuration

There are only two places to configure things, split by whether a change needs a restart.

### Runtime config → admin panel, Settings page

Saving takes effect **immediately**, no restart. Values live in the database and take precedence over `config.json` and command-line flags.

| Item | Notes |
|---|---|
| Default model | used when the client doesn't send `model` |
| Per-slot concurrency / RPM / RPH | rate limits, 0 = unlimited |
| Prompt byte cap | over the cap: with a cookie the history is sent as a text attachment (usable length up to ~160,000 bytes), without one the request is rejected with 400 `context_length_exceeded`. Neither path truncates silently. Counted in UTF-8 bytes (the upstream limit is byte-based, not token-based), default 128000. 0 = unlimited |
| Multi-turn (`multi_turn`) | off by default. When on, uses Gemini's native conversation_id server-side continuation — the client resends full history each turn, the server detects the continuation and sends only the newest message while history stays server-side, so long conversations no longer hit the single-request byte wall. Works signed-in or anonymous. **Note**: it does not enlarge the model's context window; content past the window is still evicted (recent-kept sliding window). It solves "long chat without hitting the wall + keep recent context", not "feed a huge document". |
| Retry attempts / retry delay / upstream timeout | |
| Detail retention days | only request details expire; aggregates are kept forever |
| TLS fingerprint | `chrome_146` (default) / `chrome_144` / `chrome_133` / `firefox_147` / `safari_16_0` / `safari_ios_17_0` |
| Gemini `bl` version | upstream frontend build id, change it here when it expires |
| Static proxy | fallback when the proxy pool is empty; normally use the Proxy pool page |
| Request logging | |

Every value is range-checked on the server (for example `retry_attempts` accepts 1-10, timeouts 5-600 seconds) and illegal values are rejected with a reason — browser-side limits are trivial to bypass, so the real gate is server-side.

### Credentials → also in the panel

| Item | Notes |
|---|---|
| API key | generated on first boot, rotatable or replaceable from the panel |
| Google cookie | paste it on the Settings page, effective on save. Once set, `gemini-3.1-pro` appears in the model list |

Both live in the database. Saved values are never echoed back (the cookie only reports how many cookies were recognised and whether the key ones are present).

### Deploy-time config → `docker-compose.yml`

What's left is only what requires restarting the process:

| Item | Where |
|---|---|
| Listening port | `ports` + `command: --port` |
| Database path | `volumes` + `command: --db` |
| `ADMIN_TOKEN` | `environment`, the panel login token |

The `API_KEY` environment variable pins the API key (the panel can't change it), for deployments that must not be changed at runtime.

`--proxy` and `--cookie-file` are **seed parameters, not a second configuration layer**: at startup their values are imported into the proxy pool / cookie pool (deduplicated by URL and by cookie contents), and everything is managed from the panel afterwards. Change them and restart to take effect.

Command-line flags still work and are meant as temporary overrides during local debugging. Precedence: **panel changes > CLI flags / `config.json` > built-in defaults**.

When running the binary directly, copy `config.example.json` to `config.json` as a starting template. Without `--config` the program looks for `./config.json` first, then `$HOME/.config/gemini-web2api/config.json`. Tunables in that file only seed the first boot; once changed in the panel, the panel wins.

All flags:

| Flag | Notes |
|---|---|
| `--port` | listening port, default 8083 |
| `--config` | path to `config.json` |
| `--db` | SQLite path, default `./data/gemini.db` |
| `--admin-token` | panel login token; empty = no auth (only acceptable when bound to 127.0.0.1) |
| `--api-key` | pins the `/v1/*` key so the panel can't change it, same as the `API_KEY` env var |
| `--cookie-file` | imports the cookie in this file into the cookie pool at startup |
| `--proxy` | imports this proxy into the proxy pool at startup |
| `--impersonate` | TLS fingerprint profile |
| `--version` | print version and exit (this is what the Docker healthcheck runs) |

## Cookie (optional)

Attaching a Google account cookie makes requests run as a signed-in session. What it buys you is **`gemini-3.1-pro` plus its reasoning chain** (see the reasoning section above). Verified on a free account: six consecutive calls all reported `3.1 Pro`.

> Signed-in requests must carry an extra XSRF token. The project fetches it from the Gemini page automatically, caches it per cookie and re-fetches on expiry — nothing to configure. (Missing it makes **every** request fail with 400 while anonymous traffic keeps working — an earlier version hit exactly that.)

A signed-in session unlocks `gemini-3.1-pro` + reasoning chain, image/video input, **image generation (`gemini-image`) / music (`gemini-music`)** (see "Image & music" above), **canvas (`gemini-canvas`)** — an interactive HTML document returned inline — and **video generation (`gemini-video`, needs a Pro account)**. Deep research also needs a signed-in session but **is not implemented here yet**.

1. Sign in to [gemini.google.com](https://gemini.google.com)
2. DevTools (F12) → Application → Cookies → `https://gemini.google.com`
3. Copy: `SID` / `HSID` / `SSID` / `APISID` / `SAPISID` / `__Secure-1PSID`
4. Add it in the panel under Cookie pool → Add account, or write it to `cookie.txt` and start with `--cookie-file cookie.txt` (imported into the pool at startup, managed from the panel afterwards):
```
SID=...; HSID=...; SSID=...; APISID=...; SAPISID=...; __Secure-1PSID=...
```

The JSON form `{"cookie": "SID=...; ...", "sapisid": "..."}` is also accepted. Requests carrying `SAPISID` get a computed `SAPISIDHASH` authorization header, so that entry cannot be missing.

**The Cookie pool page is the only place cookies live.** Each request picks an enabled account least-recently-used first and advances the rotation. As soon as the pool holds an enabled account, `gemini-3.1-pro` appears in the model list.

An account's "failure count" and "last success" columns update automatically. Only **401/403 counts as the cookie's fault** — network errors, proxy failures and Google blocks (302) are excluded, because residential exits degrade often enough that counting them would turn the failure count into proxy noise and make healthy cookies look like the worst offenders.

Note that "last success" only means a request involving this cookie succeeded; it does **not** prove the cookie is still valid. An expired cookie doesn't error — Gemini just treats you as anonymous. Use the **Check** button in the list to tell "still signed in" from "expired/invalid" without spending a real conversation on it.

**Cookies renew themselves.** Nearly every upstream response refreshes `SIDCC` / `__Secure-1PSIDCC` / `__Secure-3PSIDCC` via `Set-Cookie`, and we merge those back into the account; separately, a keepalive ping goes to `accounts.google.com/RotateCookies` every 10 minutes (the interval is dictated by the server). (that response refreshes the same three entries).

**Each account is pinned to its own exit.** If the cookie pool and proxy pool rotated independently, one Google account would emit requests from dozens of different IPs, which is exactly what account sharing looks like to Google. An account binds to the first exit it uses and stays there until that exit becomes unusable.

**A dead account no longer kills the request** — the next account in the pool is tried instead. Otherwise a bigger pool would only mean more ways to hit a bad one.

## Proxy pool (the core of running this for free)

**Why you need it**: a single IP sending in bursts eventually gets redirected to `google.com/sorry/index`. That threshold is **80-180 requests**, and the wide range is driven by **connection strategy, exit quality and pacing** together:

| Exit | Connection strategy | Concurrency | Pacing | Successful requests when blocked |
|---|---|---|---|---|
| residential | Reused connection pool | 10 | no delay | 151 / 172 / 177 |
| residential | Reused connection pool | 3 | no delay | 103 / 111 |
| residential | Fresh connection each time | 10 | no delay | 106 / 109 |
| residential | Fresh connection each time | 1 | 24s apart | 81 / 166 |
| static | Reused connection pool | 10 | no delay | 188 |
| static | Reused connection pool | 1 | **10 per minute** | **800, never blocked** |

Only a 302 to `/sorry/` counts as blocked, and every exit was pre-screened.

**Connection reuse is worth about 60%**: concurrency pinned at 10, both arms started together, each run lasting only 80 seconds (too short for exits to drift) — reused connections reached 172/177, fresh connections 106/109.

**A steady pace matters more than anything else.** On the same static IP, a burst run was blocked after 188 requests, while a steady 10 requests/minute ran **800 requests over 110 minutes without ever being blocked**. The default `per_ip_rph=80` is therefore a very conservative floor; a deployment that deliberately paces itself can raise it a lot.

> This section previously said "slowing down does not help", based on the burst arm (103-177) and the slow arm (81-166) overlapping almost completely on residential exits. The observation was right but the attribution was wrong: residential exits degrade over long runs (6 of 8 pre-screened exits exceeded a 40% failure rate partway through the slow run), and that degradation swamped the effect of pacing. Once a static IP removes that confound, pacing turns out to matter a great deal.

**Proxy failures consume the budget early**: the dirtier the path, the sooner the block, because some of those "failed" requests did reach Google and were counted (the slow arm actually sent 195 requests to get 166 successes, 17% more than the success count suggests). Don't expect to squeeze out capacity by retrying failures.

**Once blocked, the block is hard, and it clears after roughly two hours.** Two independent re-probes of 30 requests each, 20s apart, spanning about 10 minutes: **60 requests, zero successes**. Continued probing recovered somewhere between **106 and 121 minutes**. That is where the proxy pool's default 120-minute cooldown comes from.

Control group: 10 IPs × 50 requests each (418 requests) produced zero rejections from Google. The default `per_ip_rph=80` sits at the low end of the measured burst range.

**The fix**: add several proxies on the Proxy pool page. **Each proxy is an independent IP slot** with its own concurrency/RPM/RPH allowance, so N proxies means N times the total capacity.

Supported proxy schemes:
- `http://user:pass@host:port`
- `https://user:pass@host:port`
- `socks5://user:pass@host:port`
- `socks5h://user:pass@host:port` (remote DNS resolution, avoids local DNS poisoning)

**Scheduling rules**:
- Once any proxy is configured, requests **never fall back to a direct connection** (so a full pool can't end up burning the host IP)
- 5 failures trip the circuit breaker (resettable from the panel)
- All proxies full → HTTP 429 (no Google quota consumed; retry once a slot frees up)

**Environment variables `HTTPS_PROXY` / `ALL_PROXY` are ignored.** Proxies come only from the proxy pool (`--proxy` / `config.json` merely seed the pool at startup). Otherwise a stray `export` on the host would silently change the exit IP while the panel still showed a direct connection, which makes troubleshooting misleading.

## Fingerprint simulation

Direct connections (no proxy) go through [bogdanfinn/tls-client](https://github.com/bogdanfinn/tls-client): the TLS handshake, HTTP/2 SETTINGS frames and ALPS all match a real Chrome 146. From Google's risk-control point of view this is indistinguishable from a real browser.

Proxied connections switch to stdlib `net/http` with `http.ProxyURL` (best compatibility), but the application-layer headers (`Sec-CH-UA`, `Sec-Fetch-*`, `User-Agent`) are still spoofed with Chrome 146's real values.

**Note that through a proxy the TLS fingerprint is still ours, not the exit node's.** HTTP CONNECT only builds a tunnel; the TLS handshake is end-to-end between us and Google. JA3 echo tests confirm it: through the same proxy, stdlib and tls-client produce different JA3s (so the proxy isn't rewriting anything), while stdlib direct and stdlib through a proxy produce identical JA3s (so the proxy layer doesn't touch TLS). **So once a proxy is configured, what Google sees is the Go standard library's fingerprint, not Chrome 146's.**

This does not change the blocking threshold, though: in a same-start comparison, tls-client Chrome_146 reached 111 requests and Go stdlib 103 — a gap of 8, while the variance between exits under identical settings reaches 36% (151 vs 111). Making the proxy path use the Chrome fingerprint needs a different justification (long-term account profiling, say); it won't buy throughput.

**An observation from testing**: a naive SDK call (e.g. Python urllib) that trips risk control gets **HTTP 429**; once disguised as Chrome, tripping it yields **HTTP 302** to `google.com/sorry/index` (the CAPTCHA page). Both are IP blocks, but the 302 confirms Google really does take us for a browser.

## Privacy

- **Prompt and response bodies are never stored** — only metadata: model, proxy id, latency, token counts, status code, error type
- Legacy `prompt_preview` / `response_preview` columns are dropped automatically when migrating from older versions
- Token counts are computed with the [tiktoken-go](https://github.com/pkoukk/tiktoken-go) cl100k_base BPE tokenizer (accurate for both English and Chinese), not a `len/4` estimate
- Gemini's web app does not return real token counts (no token/usage field appears anywhere in the response), so local estimation is the only option

## OpenAI protocol coverage

| Endpoint | Status | Notes |
|---|---|---|
| `POST /v1/chat/completions` | ✅ | Real streaming: every upstream frame is forwarded as a delta (a 400-character Chinese answer produced 40 chunks). Chunk sequence is `delta{role}` → `delta{content}`×N → empty `delta` + `finish_reason` → `[DONE]`. **Degrades to buffered when `tools` are present** — a tool_call block needs the complete text to parse |
| `POST /v1/responses` | ✅ | OpenAI Responses API (used by Codex CLI). **Not incrementally streamed** — still buffered, then emitted as the event sequence |
| `GET /v1/models` | ✅ | 2 models anonymously, the third appears once a cookie is set |
| `GET /` | ✅ | Health check, unauthenticated, returns status/version/models |
| `/v1beta/models/…` (Gemini CLI native) | ❌ | Not implemented, only OpenAI-shaped endpoints are exposed |
| `/v1/embeddings`, `/v1/images/*`, `/v1/audio/*` | ❌ | Not implemented, return 404 |
| Function calling | ⚠️ | Prompt-level implementation (the model emits a ` ```tool_call``` ` block that we parse with a regex), not a real protocol layer. **Reliable for looking up private data or internal systems**, but anything Gemini can answer itself (the weather) gets answered directly, and side-effecting actions (sending email) are refused. **Agentic clients (Codex, etc.) now work**: before 4.11.0 their tens-of-KB system prompts buried our tool instructions and caused off-topic replies; fixed now. Since 4.12.0 tool results are condensed into a clear success/failure signal, so weak models no longer misread a no-output success as a failure and loop retrying (measured: the same task dropped from 26 loops to 2-4 before it concludes) |
| `tool_choice` | ⚠️ | `none` injects no tool definitions at all; `required` and a named function add mandatory wording and drop the other tools from the prompt. A prompt-level layer **cannot truly force it** — in testing, questions the model can answer itself (weather, 2+2) were answered directly even under `required` |
| `stream_options.include_usage` | ✅ | Emits a usage chunk with an empty `choices` array after `finish_reason` |
| `usage` token counts | ✅ | tiktoken cl100k_base, the same basis as the panel's requests table |
| `n` > 1 | ❌ | Returns 400. Upstream yields a single candidate; silently treating it as 1 would short-change the client |
| Sampling params | ➖ | `temperature` / `top_p` / `max_tokens` / `stop` / `seed` / `presence_penalty` / `frequency_penalty` are **accepted and ignored, not rejected**. Gemini's web protocol has no such knobs |
| `response_format` / `logprobs` | ➖ | Not implemented, accepted and ignored |
| Vision / image input | ⚠️ | **Works with a cookie**: both `image_url` (chat) and `input_image` (responses) are accepted, from `data:` URLs or http(s) links, up to 12MB per image. Anonymous returns 400 — anonymous upload **does** work (`content-push.googleapis.com/upload/`, two-step resumable), but referencing the uploaded file in a conversation is refused upstream (`BardErrorInfo 1100`) |
| Audio | ❌ | Sending `input_audio` returns 400. The web app has music generation (Lyria 3), signed-in only |

## Project layout

```
main.go                    just calls app.Run(); the entry stays at the root so `go build .` works
internal/app/              everything else
  app.go                   flag parsing + route registration + startup
  config.go                config loading + defaults
  client.go                tls-client (chrome146) + stdlib (when proxied)
  gemini.go                model table + 80-slot payload + model header + StreamGenerate + wrb.fr parsing
  xsrf.go                  XSRF token required with a cookie: fetch + per-cookie cache + self-heal
  messages.go              OpenAI messages → prompt, tool_call parsing
  server.go                /v1/* + rate-limit entry + request validation + metrics
  sse.go                   SSE writer (lazy header, so early failures still return 502 JSON)
  ratelimit.go             per-IP-slot concurrency / RPM / RPH
  tokenizer.go             tiktoken cl100k_base singleton
  apikey.go                API key (locked by flag / rotatable from the panel)
  db.go                    SQLite schema: sessions / requests / accounts / kv
  proxy.go                 proxy pool CRUD + capacity scheduling + circuit breaker
  cookie_pool.go           cookie pool data layer (CRUD + least-recently-used pick + health writeback + refresh merge)
  rotate.go                session keepalive (accounts.google.com/RotateCookies, server-dictated interval)
  upload.go                attachment upload (content-push, two-step resumable)
  context_file.go          over-long conversations as a text attachment
  vision.go                image input: data: URL / http(s) link -> pending attachment
  bl.go                    upstream frontend version (bl) auto-follow
  scheduler.go             hourly/daily aggregation + retention cleanup
  runtime.go               runtime config snapshot (panel edits apply instantly)
  admin.go                 /admin/api/* auth + REST
  admin_cookies.go         cookie pool admin REST (redacted before returning)
  admin_ui.go              embeds admin_ui/
  admin_ui/                admin panel frontend (single page + Chart.js, embedded, no CDN)
  gemini_test.go           protocol-layer tests: payload slots, wrb.fr parsing, model gating, rate limits
Dockerfile                 multi-stage (alpine builder → distroless runtime)
docker-compose.yml         single container, pulls the ghcr image by default, sqlite on a volume
.github/workflows/         docker.yml pushes images / release.yml publishes binaries on tag
```

## Limitations

- **Per-IP ceiling**: when sending in bursts, measured at **80-180 requests** before the sorry-page redirect (connection reuse buys about 60%: at concurrency 10, a reused pool reached 172/177 versus 106/109 for a fresh connection per request). But **a steady pace barely reaches the ceiling at all** — 10 requests/minute on a static IP ran 800 requests without a block. `per_ip_rph=80` sits at the bottom of the burst range → use the proxy pool to scale, or pace yourself and raise the limit
- **Signed-in features**: image generation (`gemini-image`), music (`gemini-music`), canvas (`gemini-canvas`) and video generation (`gemini-video`, Pro accounts only — free accounts are refused by the content policy) are implemented; deep research is not (a multi-step async flow)
- **Function calling**: prompt-level, the model doesn't always answer in the expected format (a real protocol layer isn't available to us)
- **Multimodal**: image/video input needs a cookie. Image, music, canvas and video generation work with a cookie; video generation additionally needs a Pro/paid account
- **Long context hits two walls**: ~130,000 bytes for the request body and ~160,000 bytes for attachments (the latter is the **total** amount of content the model can see — splitting it across several attachments does not raise the budget). A cookie only takes the usable length from 130K to ~160K; genuinely long conversations still have to be compacted by the client
- **Token counts**: tiktoken estimates (Gemini's real tokenizer is not public), within about ±20% of the true value
- **Cookie pool never auto-removes a bad account**: outcomes are written back (only 401/403 count as the cookie's fault — network errors and 302 blocks don't), but failures never trigger an automatic disable, so you have to do it from the panel. Also `last_ok_at` only means "a request using this cookie succeeded", not that the cookie is still valid — an expired cookie doesn't error, Gemini just treats you as anonymous and plain text requests still return 200
- **Streaming is only half real**: `/v1/responses` and chat requests carrying `tools` are buffered; only plain chat streams incrementally

## Troubleshooting

| Symptom | Most likely cause | What to do |
|---|---|---|
| We return **429** | Every IP slot is at its concurrency/RPM/RPH limit. **This is not Google refusing** and no upstream quota was consumed | Add proxies, or raise the limits on the Settings page |
| Panel diagnostics show **302 → `google.com/sorry/index`** | This exit IP is blocked by Google (after 80-180 requests, depending on connection strategy and exit quality) | Change exit / add proxies. **The block is hard, not probabilistic** (60 probes after a block, zero successes), so retrying in place is pointless |
| Occasional empty responses, logged as an upstream refusal | Upstream transient refusal (`1155`). There is no predictable threshold — it correlates with neither rate, concurrency, nor cumulative count | Resend once and it usually works. **Lowering RPM does not help**: measured, it is unrelated to request rate |
| Every request times out | This host can't reach `gemini.google.com` | Configure a proxy (panel or `--proxy`). Note that **`HTTPS_PROXY` is not read** |
| Exits at startup with `unable to open database file (14)` | The container runs as nonroot (uid 65532) but the bind-mounted host directory is owned by root, so it can't be written | Use a named volume (the default in compose), or `sudo chown -R 65532:65532 ./data` |
| `gemini-3.1-pro` errors out immediately | It isn't exposed without a cookie, by design | Add an account on the Cookie pool page and it becomes available |
| Every request returns 502 after attaching a cookie | The cookie expired, so the XSRF token can't be fetched | Re-export the cookie. Quick check: if `gemini-3.1-pro` reports 3.5 Flash-Lite, it's expired |
| Panel won't open / 401 | `--admin-token` (or `ADMIN_TOKEN`) doesn't match | An empty token disables auth, which is only acceptable when bound to 127.0.0.1 |

With Docker's default bridge network, upstream may return empty content (Google rejects certain NAT ranges). We have **never reproduced it here**. If you hit it, try `network_mode: host` to confirm whether that is the cause.

## Acknowledgments

- [bogdanfinn/tls-client](https://github.com/bogdanfinn/tls-client) — real Chrome TLS fingerprints
- [pkoukk/tiktoken-go](https://github.com/pkoukk/tiktoken-go) — BPE tokenizer
- [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) — pure-Go SQLite (CGO-free, builds straight on alpine)

## License

MIT — see [LICENSE](LICENSE)

## Links

- [Sophomoresty/gemini-web2api](https://github.com/Sophomoresty/gemini-web2api) — a Python implementation of the same idea; its approach informed our early work on the Gemini web protocol
- [LINUX DO](https://linux.do) — the community where this project is shared

[![LinuxDo](https://img.shields.io/badge/%E7%A4%BE%E5%8C%BA-LinuxDo-blue?style=for-the-badge)](https://linux.do/)

