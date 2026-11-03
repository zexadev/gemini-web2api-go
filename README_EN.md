# gemini-web2api-go

**Gemini web → OpenAI-compatible API.** A self-hosted proxy that turns the free [gemini.google.com](https://gemini.google.com) web app into `/v1/chat/completions` — **no Google API key, no paid quota**. Single Go binary, real Chrome TLS fingerprint, cookie pool with automatic renewal, proxy pool, built-in admin dashboard, SQLite / MySQL / PostgreSQL. Image, music, canvas and video generation (`/v1/videos`) included. Models track the web app, **Gemini 4 Argon** included (needs a paid-account cookie).

<img src="docs/banner.svg" alt="gemini-web2api-go — Gemini web to OpenAI-compatible API, native Gemini API and MCP" width="100%">

[![Release](https://img.shields.io/github/v/release/zexadev/gemini-web2api-go?label=release)](https://github.com/zexadev/gemini-web2api-go/releases/latest)
[![Downloads](https://img.shields.io/github/downloads/zexadev/gemini-web2api-go/total?label=downloads)](https://github.com/zexadev/gemini-web2api-go/releases)
[![Stars](https://img.shields.io/github/stars/zexadev/gemini-web2api-go?style=flat&label=stars)](https://github.com/zexadev/gemini-web2api-go/stargazers)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.21%2B-00ADD8.svg)](https://golang.org)
[![Docker](https://img.shields.io/badge/docker-distroless-blue)](Dockerfile)

[中文文档](README.md) | English

> Official repository: **[github.com/zexadev/gemini-web2api-go](https://github.com/zexadev/gemini-web2api-go)** — releases, issues and docs live here; forks are not maintained by this project.

Turn the Google Gemini web app into an OpenAI-compatible API. **Single binary**, **no account needed** (anonymous works), **real Chrome 146 fingerprint**, **SQLite / MySQL / PostgreSQL**, ships with an **admin dashboard**.

---

## Contents

- [What this is](#what-this-is)
- [Features](#features)
- [Quick start](#quick-start)
- [Connecting clients](#connecting-clients)
- [Gemini native API](#gemini-native-api)
- [MCP tools](#mcp-tools)
- [Admin panel](#admin-panel)
- [Models](#models)
- [Configuration](#configuration)
- [Cookie (optional)](#cookie-optional)
- [Proxy pool (the core of running this for free)](#proxy-pool-the-core-of-running-this-for-free)
- [Fingerprint simulation](#fingerprint-simulation)
- [Privacy](#privacy)
- [OpenAI protocol coverage](#openai-protocol-coverage)
- [Project layout](#project-layout)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)
- [FAQ](#faq)
- [Star History](#star-history)
- [Acknowledgments](#acknowledgments)
- [License](#license)
- [Links](#links)

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

### Compared with similar projects

The projects most often compared with this one are the Python [Sophomoresty/gemini-web2api](https://github.com/Sophomoresty/gemini-web2api), the Python library [HanaokaYuzu/Gemini-API](https://github.com/HanaokaYuzu/Gemini-API) and [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). Going by their READMEs (2026-11-03), listing only what those READMEs let us verify:

| | gemini-web2api-go (this repo) | Sophomoresty/gemini-web2api | HanaokaYuzu/Gemini-API | CLIProxyAPI |
|---|---|---|---|---|
| Upstream | gemini.google.com web protocol, no API key | Same | Same | Accounts on official channels: Gemini API / AI Studio / Gemini CLI / Antigravity (OAuth / API key) |
| Form | Single Go binary, distroless container image | Single Python file | Python library (imported in code) + CLI | Go service |
| API surface | OpenAI-compatible + native Gemini + `/v1/videos` + MCP tools | OpenAI-compatible + native Gemini | Python API, no HTTP server | OpenAI / Gemini / Claude compatible |
| Multiple accounts | Cookie pool with rotation, automatic renewal, each account pinned to its own exit | One cookie file (`cookie_file` + `auth_user`) | One cookie set, refreshed in the background | Multi-account load balancing |
| Exit IPs | Proxy pool: circuit breaker, rotation, each proxy its own rate-limit slot | One proxy | One proxy | — (official API quota) |
| Silent downgrade | `strict_model`: a downgraded answer does not count, retry on the next account, `429` when the whole pool is downgraded | README notes that a free account asking for Pro gets Flash | Not mentioned in README | — (official API quota) |
| Admin panel | Built in | Not mentioned in README | Not mentioned in README | Companion project CPAMC, installed separately |
| Gemini 4 Argon | With a paid-account cookie, two thinking tiers | Not mentioned in README | Models discovered per account, not named in README | Yes, depending on channel |

## Features

**API**
- OpenAI-compatible: `/v1/chat/completions`, `/v1/models`, `/v1/responses`
- Gemini native format: `/v1beta/models/{model}:generateContent` / `:streamGenerateContent`, usable directly by Google's SDKs and gateways' Google providers, function calling included (see [Gemini native API](#gemini-native-api))
- `/v1/videos`: OpenAI (Sora)-shaped async video generation — `POST` to create, `GET /v1/videos/{id}` to poll, `GET /v1/videos/{id}/content` to download the MP4 (needs a signed-in Pro cookie)
  - image → video: `input_reference` (start frame) as a data/http(s) URL, `{"image_url": …}`, or a multipart file (OpenAI SDK shape)
  - `size` (`1280x720` / `720x1280`) or `aspect_ratio` (`16:9` / `9:16`)
  - `POST /v1/videos/{id}/extend` `{"prompt": "what happens next"}` continues a finished video in its own Gemini conversation (same account) and returns the whole lengthened MP4. Needs `temporary_chat` and `auto_delete_conversation` both off (the conversation has to stay in the account)
  - failed jobs say why at once: `video_quota_exhausted` (every account's video budget is used up, see below) and `video_refused: <Gemini's reply>` (Gemini answered with text instead of a video)
  - video quota routing: an account whose video budget runs out is skipped for new videos until it resets, and a job that lands on one moves to the next account inside the same job. When every account is cooling down, `POST /v1/videos` answers `429` with `code: video_quota_exhausted` and `Retry-After`, and a running job fails with `retry_after=N` (seconds until the first reset)
  - `GET /admin/api/video-quota` (admin): per-account video usage in the rolling 5-hour window, status and reset time, read from each account's own Gemini history (read-only; Google's own reset time when a refusal carries it, otherwise first video + 5 h). Each refresh costs about 2 + N upstream requests per account (N = conversations updated in the last few hours; unchanged ones are cached), sent through the proxy pool and counted against the per-IP budget, so don't poll it. With `temporary_chat` or `auto_delete_conversation` on, videos made through this proxy aren't in the history and are undercounted (the response carries a `history_incomplete` note)
- Real incremental streaming: every upstream frame is forwarded as a delta, on `/v1/chat/completions`, `/v1/responses` and requests with `tools` alike (a tool-call block is sent once complete)
- Bearer token / `x-api-key` / `x-goog-api-key` auth; the key can be rotated from the panel
- `usage` computed with tiktoken; `reasoning_tokens` counted separately, not folded
  into `completion_tokens`

**Models**
- `gemini-3.6-flash` and `gemini-3.5-flash-lite` work anonymously, web search included
- `gemini-3.1-pro` needs a cookie; every reply carries a reasoning chain
- `gemini-3.8-flash` and `gemini-4-argon` need a cookie from a paid / Ultra account
- Every model has a `-thinking` variant (extended thinking), available with a cookie
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
- SQLite persistence: 30 days of per-request detail plus permanent aggregates (optional MySQL / PostgreSQL via `SQL_DSN`)
- Admin panel: overview / requests / proxy pool / cookie pool / settings / about, with
  config changes applied without a restart and a top-bar notice when a new release is out
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

To use MySQL / PostgreSQL, just set the `SQL_DSN` env var (unset = SQLite, nothing to change):

```bash
# MySQL (also accepts the native go-sql-driver form user:pass@tcp(host:3306)/dbname)
SQL_DSN="mysql://user:pass@host:3306/dbname"
# PostgreSQL
SQL_DSN="postgres://user:pass@host:5432/dbname?sslmode=disable"
```

Tables are created automatically; all three backends share one schema. Single instance? Stick with the default SQLite. You only need MySQL/PG when several instances share one pool.

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
gemini-web2api-go v4.27.0
  Listening:   http://0.0.0.0:8083
  Base URL:    http://localhost:8083/v1
  API key:     sk-gemini-XX...XXXX  (mutable in admin UI)
  Admin UI:    http://localhost:8083/admin  (token auth)
  DB:          ./data/gemini.db
  Models:      [gemini-3.5-flash-lite gemini-3.6-flash]
  Cookie:      none (anonymous)
  Proxy:       none (direct)
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

> Got it running? A ⭐ helps: GitHub topic pages rank by stars, and that is how most people find this project. Issues and ideas are welcome, and if you have improved something in a fork, send it back as a PR.

## Connecting clients

Every OpenAI-compatible client (Cherry Studio / ChatBox / Open WebUI / dify / Cursor / …) is configured the same way:

| Field | Value |
|---|---|
| Base URL | `http://localhost:8083/v1` (some clients want just `http://localhost:8083` and append `/v1` themselves) |
| API key | the `sk-gemini-…` shown on the admin panel's Settings page |
| Model | `gemini-3.6-flash` |

**newapi / one-api channel**: pick the OpenAI type, set the base URL to `http://localhost:8083` (use `http://host.docker.internal:8083` or the container name if newapi runs in Docker too), paste the API key, and list the models as `gemini-3.6-flash,gemini-3.5-flash-lite`.

**Codex CLI** uses `/v1/responses` — point its base URL at `http://localhost:8083/v1`.

**Google-format clients** (the `google-genai` SDK, gateways' Google providers) use `/v1beta`; see [Gemini native API](#gemini-native-api) below. Gemini CLI has not been tested.

There is also an unauthenticated health check at `GET /` returning `{"status":"ok","version":…,"models":[…]}`.

## Gemini native API

The same port also speaks the Google Gemini API format, for clients that only talk Google's wire format (Google's
`google-genai` SDK, the Google provider in gateways such as Bifrost):

| Path | Notes |
|---|---|
| `POST /v1beta/models/{model}:generateContent` | Non-streaming |
| `POST /v1beta/models/{model}:streamGenerateContent` | Real streaming; SSE with `?alt=sse` (the SDK default), a JSON array without it |
| `POST /v1beta/models/{model}:countTokens` | Local tiktoken estimate |
| `GET /v1beta/models`, `GET /v1beta/models/{model}` | Model list, same as `/v1/models` |

- **Auth**: send the API key as the `x-goog-api-key` header or the `?key=` query parameter (what the SDKs do);
  `Authorization: Bearer` works too.
- **Model names** are this project's (`gemini-3.6-flash`, `gemini-3.1-pro`, …, see [Models](#models)), not the official
  API's `gemini-2.5-flash` and so on.
- **Function calling**: `tools[].functionDeclarations`, `toolConfig.functionCallingConfig` (`AUTO` / `ANY` / `NONE`,
  `allowedFunctionNames`), and `functionCall` / `functionResponse` in the history are all accepted; the model's calls
  come back as `functionCall` parts, and the SDK's automatic function calling works. Underneath it is the same
  prompt-level implementation as the OpenAI endpoints, with the same limits (see the Function calling row in
  [protocol coverage](#openai-protocol-coverage)).
- **Thinking**: with `generationConfig.thinkingConfig.includeThoughts: true`, the thought summary comes back as a part
  with `thought: true` (only for models that produce one).
- **Image / video input**: `inlineData`, or `fileData` with an http(s) link; needs a cookie. There is no Files API, so
  `files/…` URIs and other types (PDF, audio) return 400.
- **Image / music generation**: with `gemini-image` / `gemini-music`, the output comes back as `inlineData` parts.
- Sampling settings in `generationConfig` (`temperature`, `maxOutputTokens`, …) are accepted and ignored;
  `candidateCount` > 1 returns 400.
- Errors use Google's shape `{"error":{"code","message","status"}}`, with the same status codes as the OpenAI endpoints.

```python
from google import genai
from google.genai import types

client = genai.Client(api_key="sk-gemini-...",
                      http_options=types.HttpOptions(base_url="http://localhost:8083"))
print(client.models.generate_content(model="gemini-3.6-flash", contents="Hello!").text)
```

## MCP tools

Besides the OpenAI API, the same process and port also serves an **MCP server** at `/mcp`,
exposing Gemini's web-grounded **search**, **image**, **music** and **video** generation as
tools that MCP clients (Claude Desktop / Claude Code / Cursor) can call directly.

No separate process or deployment — running the backend gives you this. The transport is
HTTP (Streamable HTTP), so remote clients just point at the URL, reusing the backend's
account pool / proxy pool / rate limiting. Search works anonymously; image / music / video
need a cookie (video needs a paid account).

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

| Tool | Arguments | Returns |
|---|---|---|
| `web_search` | `query` | Gemini's web-grounded answer with a `Sources:` list appended |
| `generate_image` | `prompt`, optional `image_urls` (reference images: data: or http(s) links) | the image (MCP `image` content) |
| `generate_music` | `prompt` | a ~30 s clip (MCP `audio` content; an embedded resource when upstream doesn't serve `audio/*`) |
| `create_video_task` | `prompt`, optional `size` (`1280x720` / `720x1280`), `input_reference` (start frame) | a task id, the same jobs as `/v1/videos` |
| `get_video_status` | `id`, optional `include_video` | the task status; with `include_video=true` on a finished task, the MP4 attached (embedded resource, several MB) |

Results use MCP's native media content types (base64 in `data` / `blob`, with `mimeType`), not
text, so clients that support them render an attachment. `url_context` (reading a specific
URL's content) is not implemented yet.

## Admin panel

`http://localhost:8083/admin`, log in with `--admin-token`.

- **Overview** — 24h KPIs, request volume / P50 latency dual-axis chart, per-model and per-proxy breakdowns, IP rate-limit usage, one-click connectivity check
- **Requests** — request log (metadata only, no prompt/response content), status/model filters, pagination
- **Proxy pool** — runtime CRUD, enable/disable, circuit breaker on repeated failures (each proxy is its own IP slot)
- **Cookie pool** — import multiple signed-in Google accounts; requests rotate through them **least-recently-used first**. One-click "check" tells you whether an account is still signed in, and **dead accounts are flagged in red** so you know which to re-import; `__Secure-1PSIDTS` is rotated automatically plus a 10-minute keepalive; each account is pinned to its own exit; when several Google accounts are signed in to one browser, the "account index" picks which one to use. The list shows redacted summaries only (cookie count / key entries / last 4 of SAPISID / failure count)
- **Settings** — runtime config form (saved settings take effect immediately), API key rotation, read-only view of deploy-time config
- **About** — current and latest version plus a manual update check; when a newer release exists it shows the release notes, the Docker upgrade command and per-platform binary download links (the binary is never replaced automatically; upgrading is still up to you)

The frontend is a single HTML file and Chart.js is embedded in the binary, **not loaded from a CDN**, so the panel also works on air-gapped or intranet deployments.

**Serving it under a path prefix needs no extra configuration**: every URL in the panel is relative, so forwarding `https://example.com/gemini/` to this service's `/` is enough to open `https://example.com/gemini/admin`. (Requesting `/admin` without the trailing slash 301s to `admin/` — relative URLs resolve against the document URL's *directory*, and the two forms differ by one level, so they have to be normalised first.)

## Models

Models follow the Gemini web app's model picker (the list comes from `batchexecute?rpcids=otAQ7b`): five base models,
of which 3.6 Flash / 3.5 Flash-Lite / 3.1 Pro work with a free account and 3.8 Flash / 4 Argon need a paid one; the rest are their thinking tiers and the media tools.

| Model | Description |
|---|---|
| `gemini-3.6-flash` | All-round, default |
| `gemini-3.5-flash-lite` | Fast and lightweight |
| `gemini-3.1-pro` | Most capable, **needs a cookie**; every reply carries a reasoning chain |
| `gemini-3.8-flash` | Newer Flash, **needs a cookie from a PAID Gemini account** (free accounts get downgraded to 3.5 Flash-Lite). `gemini-3.7-flash` is an alias — Google bumped that same hex from 3.7 to 3.8 in place |
| `gemini-4-argon` | 4 Argon (complex tasks), **needs a cookie from a PAID/Ultra Gemini account** (free accounts get downgraded to 3.5 Flash-Lite) |
| `gemini-3.6-flash-thinking` | 3.6 Flash with extended thinking; **needs a cookie** |
| `gemini-3.5-flash-lite-thinking` | 3.5 Flash-Lite with extended thinking; **needs a cookie** |
| `gemini-3.1-pro-thinking` | 3.1 Pro with extended thinking; **needs a cookie** |
| `gemini-3.8-flash-thinking` | 3.8 Flash with extended thinking; **needs a paid-account cookie** (`gemini-3.7-flash-thinking` is an alias) |
| `gemini-4-argon-thinking` | 4 Argon with extended thinking (upstream "High" tier), carries `reasoning_content`; **needs a PAID/Ultra account cookie** |
| `gemini-4-argon-thinking-medium` | 4 Argon at the Medium thinking tier (thinks server-side, no `reasoning_content`); **needs a PAID/Ultra account cookie** |
| `gemini-image` | Image generation (Nano Banana); base64 output; **needs a cookie** |
| `gemini-music` | Music (Lyria, ~30s); base64 output; **needs a cookie** |
| `gemini-canvas` | Canvas: generates an interactive HTML document (returned inline as a ```html block); **needs a cookie** |
| `gemini-video` | Video generation (async, tens of seconds to a few minutes); base64 MP4 output; **needs a Pro/paid account** (free accounts are refused by the upstream content policy) |

Without a cookie, `/v1/models` returns only the first two, and asking for `gemini-3.1-pro` fails with an explanation. An anonymous request for it is always silently downgraded to 3.5 Flash-Lite — better to fail at model selection than to hand back a reply that "succeeded" but isn't Pro.

With a valid cookie it really is Pro: six consecutive calls all had the backend report `3.1 Pro` itself.

The `-thinking` variants are the web UI's "extended thinking" switch, orthogonal to the model, not extra models. The backend then reports a `High` suffix (e.g. `3.1 Pro High`; earlier upstream called it `Extended`). **Only effective when signed in**: anonymous requests have the switch silently ignored, so they aren't exposed without a cookie.

The old names `gemini-3.5-flash`, `gemini-3.5-flash-thinking`, `gemini-3.5-flash-thinking-lite`, `gemini-auto` and `gemini-flash-lite` were **removed** (they now return 400): the backend has no entries for them, and keeping them only suggested there were five distinct models to choose from.

> **`@think=N` is deprecated.** The suffix was written into `inner[17]` and long treated as "thinking depth", but captures show it is the **turn index within a conversation** (first turn `[[0]]`, the follow-up carrying a conversation id `[[1]]`, incrementing from there) — nothing to do with reasoning depth, so the parameter never did anything. The suffix is still accepted and ignored, so existing client configs don't break.

### Reasoning chain (`reasoning_content`)

`gemini-3.1-pro` emits its own reasoning before every answer. The project exposes it
as `reasoning_content`, the de-facto standard field, so clients like newapi, Cherry
Studio and ChatBox render it as a collapsible "thinking" panel. 3.6 Flash and 3.5 Flash-Lite
at the normal tier produce no reasoning chain, and the field is then omitted entirely.

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

Attaching a cookie additionally unlocks `gemini-3.1-pro`, **the extended-thinking (`-thinking`) variants**, **image / video input**, **a longer context** (over-long conversations are sent as a text attachment, see below), and **image generation (`gemini-image`), music (`gemini-music`), canvas (`gemini-canvas`), video generation (`gemini-video`, needs a Pro account)**.

Deep research is still **not implemented** (a multi-step async flow). The panel's "actual model" column always shows which model the backend really used, so any downgrade is visible.

### Image & music

`gemini-image` (Nano Banana) and `gemini-music` (Lyria, ~30s) go through `/v1/chat/completions` just like a normal chat — put the image/music description in the user message. The bytes come back as a **base64 data URL** inside `content`: images as `![image](data:image/png;base64,…)`, audio as `[audio](data:audio/mpeg;base64,…)`. Markdown-capable clients render the image inline; to save a file, decode the base64 after the comma.

```bash
curl http://127.0.0.1:8083/v1/chat/completions \
  -H "Authorization: Bearer sk-gemini-..." -H "Content-Type: application/json" \
  -d '{"model":"gemini-image","messages":[{"role":"user","content":"an orange cat in an astronaut helmet"}]}'
```

Not returning a URL is deliberate — Gemini's artifact links need the cookie to download, so a bare link would be dead on the client side; the server fetches the bytes and inlines them. The base64 is **not counted in `completion_tokens`** (a single image is millions of tokens; billing that downstream would be absurd), only the caption text is. Both need a cookie, so they don't appear in `/v1/models` without one. Params like `size` / `n` have no upstream knob and are ignored.

**For bigger, sharper images**, turn on **Full-size image download** (`image_upscale`) on the admin panel's Settings page. Images are then fetched the way the web app's Download button does it: on **Ultra accounts** the image is upscaled 2× in each dimension (1376×768 → 2752×1536, about 3-4.5 MB); **other accounts** don't get the upscale, but they get the original file instead of the recompressed one (same size, about 0.8 MB vs about 0.2 MB by default). It costs one extra request per image and a several-times larger response. If it fails, the default image is returned; nothing is regenerated.

**By default, multi-turn context is sent by flattening `messages` into a single prompt** (turn on server-side multi-turn, `multi_turn`, in Settings and a recognised continuation sends only the latest message while the history stays server-side; see [Configuration](#configuration)). The cost of flattening is that every turn resends the whole history, which runs into the single-request length wall: about **130,000 UTF-8 bytes**. Past that the upstream **silently truncates from the tail without an error** — and since the newest message sits at the end, what gets eaten is exactly what you just asked, which reads as "the model suddenly got dumb".

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
| Auto-delete conversation (`auto_delete_conversation`) | off by default. When on, each finished request deletes the conversation it left on gemini.google.com, so signed-in accounts don't pile them up. Signed-in only; async best-effort, a failed delete doesn't affect the response. |
| Temporary chat (`temporary_chat`) | off by default. When on, uses Gemini web's native **Temporary Chat** (`inner[45]=1`): the conversation never enters the account's chat list and isn't used for model training — cleaner than auto-delete, and it saves that extra delete request. Only meaningful signed-in; automatically inactive when server-side multi-turn is on (a temporary chat leaves no list entry, while multi-turn relies on the server keeping the conversation). |
| Full-size image download (`image_upscale`) | off by default. When on, generated images are fetched the way the web app's Download button does: upscaled 2× in each dimension on Ultra accounts, the original (not recompressed) file on other accounts. One extra request per image; falls back to the default image if it fails. See "Image & music" above. |
| Anonymous-first (`anon_first`) | off by default. When on, requests that don't need a signed-in session (plain text, no thinking, no tool, no image) go out anonymously without consuming a cookie account, saving account quota; only 3.1 Pro / 3.8 Flash / extended thinking / image gen / file upload etc. pick an account. When off, any request uses an account as long as the pool has one. |
| Strict model check (`strict_model`) | off by default. When an account runs out of quota for a tier, upstream doesn't error — it silently switches to a weaker model and still returns 200. When on, if the model the server reports differs from the one requested, the downgraded answer is not treated as a success: the request retries on the next account, and if the whole pool is downgraded it returns `429 insufficient_quota` (`code: model_downgraded`, with `Retry-After`). For streaming this is decided before the first token is sent, so you never get half an answer followed by an error. Media models (image/music/video/canvas) are not checked. |
| Downgrade cooldown (`strict_model_cooldown_min`) | Works with strict model check: how long a downgraded account rests for that model before being used again. Default 300 minutes (web usage is counted over a 5-hour window). 0 = do not remember, retry on every request. |
| Retry attempts / retry delay / upstream timeout | |
| Detail retention days | only request details expire; aggregates are kept forever |
| TLS fingerprint | `chrome_146` (default) / `chrome_144` / `chrome_133` / `firefox_147` / `safari_16_0` / `safari_ios_17_0` |
| Firefox fingerprint (`firefox_fingerprint`) | experimental, off by default. When on, requests pose as Firefox: on direct connections the TLS handshake uses a Firefox fingerprint, the UA becomes Firefox, and Chrome-only `Sec-CH-UA` headers are dropped. Recommended together with cookies exported from Firefox. Through a proxy the TLS layer can't change (see [Fingerprint simulation](#fingerprint-simulation)), only the headers do |
| Gemini `bl` version | upstream frontend build id, change it here when it expires |
| Follow `bl` automatically (`gemini_bl_auto`) | fetches the latest build id from the `/app` page every 6 hours; if that fails or looks wrong, the pinned value above is used |
| Breaker cooldown (`proxy_cooldown_min`) | how long a proxy rests after 5 consecutive failures before it returns to the pool, default 120 minutes (blocked exits were measured to recover in 106-121 minutes). 0 = never auto-recover; reset it on the Proxy pool page |
| Fall back to direct when no proxy is available (`fallback_direct`) | off by default: when every proxy is full or tripped, return an error so the host IP is never exposed upstream. On: availability first, but the upstream sees the host's real exit |
| Fall back to anonymous when the cookie is dead (`fallback_anon`) | off by default: return an error so you notice right away that the account needs replacing. On: keep going, but 3.1 Pro is silently downgraded to 3.5 Flash-Lite and the reasoning chain disappears |
| New protocol (`web_channel`) | off by default. When on, chats go through processSession (WebChannel), the interface the current web app uses, instead of the legacy StreamGenerate. Same backend, different transport. The legacy interface still works; turn this on if it breaks. Text, image input, image generation and anonymous use are tested; multi-turn resume still uses the legacy interface, and extended thinking does not take effect on the new protocol yet (the normal tier is reported). |
| Static proxy | fallback when the proxy pool is empty; normally use the Proxy pool page |
| Request logging | |

Every value is range-checked on the server (for example `retry_attempts` accepts 1-10, timeouts 5-600 seconds) and illegal values are rejected with a reason — browser-side limits are trivial to bypass, so the real gate is server-side.

### Credentials → also in the panel

| Item | Notes |
|---|---|
| API key | generated on first boot, rotatable or replaceable from the panel |
| Google cookie | import it on the Cookie pool page, effective on save; several accounts can be imported. Once the pool has an account, `gemini-3.1-pro` and the other login-only models appear in the model list |

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
3. Copy: `SID` / `HSID` / `SSID` / `APISID` / `SAPISID` / `__Secure-1PSID` / `__Secure-1PSIDTS`
4. Add it in the panel under Cookie pool → Add account, or write it to `cookie.txt` and start with `--cookie-file cookie.txt` (imported into the pool at startup, managed from the panel afterwards):
```
SID=...; HSID=...; SSID=...; APISID=...; SAPISID=...; __Secure-1PSID=...; __Secure-1PSIDTS=...
```

The JSON form `{"cookie": "SID=...; ...", "sapisid": "..."}` is also accepted. Requests carrying `SAPISID` get a computed `SAPISIDHASH` authorization header, so that entry cannot be missing.

**The Cookie pool page is the only place cookies live.** Each request picks an enabled account least-recently-used first and advances the rotation. As soon as the pool holds an enabled account, `gemini-3.1-pro` appears in the model list.

An account's "failure count" and "last success" columns update automatically. Only **failures that really are the cookie's fault** count: 401/403, a page with no signed-in session, or a redirect to the Google sign-in page. Network errors, timeouts, proxy failures and Google blocks (redirects to `/sorry`) are excluded, because residential exits degrade often enough that counting them would turn the failure count into proxy noise and make healthy cookies look like the worst offenders. **An account with 3 consecutive auth failures is disabled automatically** (not deleted: it is flagged red in the panel, re-import and enable it again).

Note that "last success" only means a request involving this cookie succeeded; it does **not** prove the cookie is still valid. An expired cookie doesn't error — Gemini just treats you as anonymous. Use the **Check** button in the list to tell "still signed in" from "expired/invalid" without spending a real conversation on it.

**Cookies renew themselves.** Two things run together:

- Nearly every upstream response refreshes `SIDCC` / `__Secure-1PSIDCC` / `__Secure-3PSIDCC` via `Set-Cookie`, and we merge those back into the account.
- Every 10 minutes (first ping 15 seconds after boot) we hit `accounts.google.com/RotateCookies`: first the sentinel payload that mints a fresh `__Secure-1PSIDTS` (the ~30-minute ticket — without it Gemini treats you as anonymous), then the browser iframe flow that refreshes SIDCC. The same account is not pinged twice within 60 seconds, to avoid HTTP 429.

> **Export cookies from Firefox.** Recent Chrome enables Device Bound Session Credentials, so a session exported from Chrome is bound to that machine and `*PSIDTS` cannot be rotated — it dies in about 30 minutes to a few hours. Firefox has no such binding, and the same keepalive will keep the session alive. If you must use Chrome: log in in a fresh private window, export immediately, and **close that window** so the browser does not rotate the same session out from under you.

**Each account is pinned to its own exit.** If the cookie pool and proxy pool rotated independently, one Google account would emit requests from dozens of different IPs, which is exactly what account sharing looks like to Google. An account binds to the first exit it uses and stays there until that exit becomes unusable.

**Several Google accounts signed in to one browser? Set the "account index".** Accounts signed in to the same browser share one cookie jar, so the exported cookie string can act as any of them; which one a request uses is decided by the URL (`/u/1/` is the second account). If you leave it unset, every request lands on the **default account** (the one signed in first). If your subscription is on another account, paid models are silently downgraded to 3.5 Flash-Lite while "check" still says the session is valid.

- In the browser, switch to the account you want and open Gemini. If the address bar shows `gemini.google.com/u/1/app`, enter `1`; no `/u/` means `0` (the default). Set it as "Account index" when importing or editing the cookie
- To use one cookie string as several accounts, import it once per account, each with its own index
- The index is the browser's sign-in order and can change if you sign out and back in; just update it if it does
- Or skip this entirely: sign out of every account in the browser, sign in to the subscribed one first so it becomes the default, then export

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
- Once any proxy is configured, requests **never fall back to a direct connection** (so a full pool can't end up burning the host IP; turn on `fallback_direct` if availability matters more)
- 5 consecutive failures trip the circuit breaker; the proxy returns automatically after `proxy_cooldown_min` (default 120 minutes), or reset it from the panel
- All proxies full → HTTP 429 (no Google quota consumed; retry once a slot frees up)
- All proxies disabled or cooling down → HTTP 502 `proxy_unavailable` (not a rate limit; wait for the cooldown or check the proxies)

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
- **The update check runs in your browser, not on the server**: opening the panel makes the browser request the latest release from `api.github.com` (cached for 6 hours), so a deployment with a proxy pool never reaches out from the host IP just to check versions. Automatic checking can be turned off on the About page

## OpenAI protocol coverage

| Endpoint | Status | Notes |
|---|---|---|
| `POST /v1/chat/completions` | ✅ | Real streaming: every upstream frame is forwarded as a delta (a 400-character Chinese answer produced 40 chunks). Chunk sequence is `delta{role}` → `delta{content}`×N → empty `delta` + `finish_reason` → `[DONE]`. With `tools`, text still streams as it arrives; only the tool_call block is held back, parsed once complete, then sent |
| `POST /v1/responses` | ✅ | OpenAI Responses API (used by Codex CLI). Real streaming, events emitted as they arrive; tool calls handled as above |
| `GET /v1/models` | ✅ | Only the 2 text models anonymously; the login-only ones are listed once the pool has a cookie |
| `GET /` | ✅ | Health check, unauthenticated, returns status/version/models |
| `/v1beta/models/…` (Google native format) | ✅ | See [Gemini native API](#gemini-native-api) |
| `/v1/embeddings`, `/v1/images/*`, `/v1/audio/*` | ❌ | Not implemented, return 404 |
| Function calling | ⚠️ | Prompt-level implementation (the model emits a ` ```tool_call``` ` block that we parse as JSON), not a real protocol layer. **Reliable for looking up private data or internal systems**, but anything Gemini can answer itself (the weather) gets answered directly, and side-effecting actions (sending email) are refused. **Agentic clients (Codex, etc.) now work**: before 4.11.0 their tens-of-KB system prompts buried our tool instructions and caused off-topic replies; fixed now. Since 4.12.0 tool results are condensed into a clear success/failure signal, so weak models no longer misread a no-output success as a failure and loop retrying (measured: the same task dropped from 26 loops to 2-4 before it concludes) |
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
  webchannel.go            new-protocol transport, processSession (WebChannel), switch web_channel
  conversation.go          server-side multi-turn (multi_turn): matches the history prefix, sends only the latest message
  media.go                 image / music / canvas / video: tool slot, artifact extraction and download
  image_upscale.go         full-size / upscaled image download (rpc c8o8Fe, switch image_upscale)
  search.go                web-search source extraction
  strict_model.go          strict model check: detects silent downgrades, cools down per (account, model)
  xsrf.go                  XSRF token required with a cookie: fetch + per-cookie cache + self-heal
  messages.go              OpenAI messages → prompt, tool_call parsing
  server.go                /v1/* + rate-limit entry + request validation + metrics
  gemini_api.go            native Gemini API /v1beta/models/… (translated onto the same chat path)
  videos.go                /v1/videos async video jobs
  video_cooldown.go        cooldown for accounts out of video quota + switching accounts within a job
  video_quota.go           /admin/api/video-quota: per-account video usage read from history
  mcp.go                   MCP server (/mcp): web_search and other tools
  mcp_media.go             MCP media tools: image / music / video
  sse.go                   SSE writer (lazy header, so early failures still return 502 JSON)
  ratelimit.go             per-IP-slot concurrency / RPM / RPH
  tokenizer.go             tiktoken cl100k_base singleton
  apikey.go                API key (locked by flag / rotatable from the panel)
  db.go                    database schema (shared by SQLite / MySQL / PostgreSQL): sessions / requests / accounts / kv
  dbdialect.go             multi-database dialects: SQL_DSN selects MySQL / PostgreSQL, placeholder rewriting
  proxy.go                 proxy pool CRUD + capacity scheduling + circuit breaker
  cookie_pool.go           cookie pool data layer (CRUD + least-recently-used pick + health writeback + refresh merge)
  rotate.go                session keepalive (RotateCookies: 1PSIDTS sentinel + SIDCC iframe, server-dictated interval)
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
- **Cookie validity is judged by auth failures**: an account is disabled automatically only after 3 consecutive auth failures (network errors and `/sorry` blocks don't count). `last_ok_at` only means "a request using this cookie succeeded", not that the cookie is still valid — an expired cookie doesn't error, Gemini just treats you as anonymous and plain text requests still return 200
- **Cookies exported from Chrome may not renew**: recent Chrome Device Bound Session Credentials bind the session to the machine, and rotating `__Secure-1PSIDTS` then returns 401. Export from Firefox for a session that can be kept alive

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
| Cookie dies in about 30 minutes; keepalive does nothing | Chrome device-bound session; rotating `__Secure-1PSIDTS` returns 401 | Re-login in **Firefox** and export again. The panel Keep-alive button should report that `__Secure-1PSIDTS` was refreshed |
| Cookie from a paid account, but paid models report 3.5 Flash-Lite and "check" says the session is valid | Several Google accounts are signed in to the browser and the subscription isn't on the default one, so requests land on the default account | Set the "account index" for that entry in the Cookie pool (`/u/1/` in that account's Gemini address bar means 1); see above |
| Panel won't open / 401 | `--admin-token` (or `ADMIN_TOKEN`) doesn't match | An empty token disables auth, which is only acceptable when bound to 127.0.0.1 |

With Docker's default bridge network, upstream may return empty content (Google rejects certain NAT ranges). We have **never reproduced it here**. If you hit it, try `network_mode: host` to confirm whether that is the cause.

## FAQ

### How is this different from the official Gemini API? Do I need an API key?

No API key. This project does not wrap `generativelanguage.googleapis.com`; it reverse-proxies the protocol of the
gemini.google.com web app itself, so there is no key and no paid quota. The trade-off is that capabilities follow the
web app: what the web app can do (web search, image, music, canvas, video generation) works here; what it can't
(deep research) doesn't. Anonymous use — no cookie at all — already gives you the text models.

### How does this relate to Sophomoresty/gemini-web2api (Python) and other Go projects named gemini-web2api?

This is an independent Go implementation. Its early protocol work was informed by the Python project (see Links);
it is **not a port**, and other Go repositories with a similar name are unrelated. What this one brings: a single
static binary, a real Chrome TLS fingerprint via utls, a proxy pool plus a cookie pool (automatic renewal, each
account pinned to one exit), a built-in admin dashboard, SQLite / MySQL / PostgreSQL, the async `/v1/videos`
endpoint, and MCP tools (search / image / music / video).

### Why does my cookie die after about 30 minutes? How do I keep it alive?

The ticket that expires is `__Secure-1PSIDTS` (roughly 30 minutes). The proxy re-mints it on a timer with a sentinel
payload and runs a keepalive, so **a cookie exported from Firefox stays alive indefinitely**. Recent Chrome enables
Device Bound Session Credentials: a session exported from Chrome returns 401 on rotation and dies within 30 minutes
to a few hours. Export from Firefox instead — no browser extension or always-on machine needed.

### Will my account get banned? Is the limit per account or per IP?

Per **exit IP + TLS fingerprint**, not per account. Measured: a burst of roughly 80–180 requests from one exit
triggers the sorry-page redirect, which clears itself after about two hours; a steady pace (e.g. 10 requests per
minute) ran 800 requests without a block. A paid plan (Google AI Pro) does **not** raise this ceiling. To scale,
spread requests across exits with the proxy pool rather than adding accounts.

### What do I get with a free account versus Pro / paid?

Anonymous (no cookie): `gemini-3.6-flash` and `gemini-3.5-flash-lite`, web search included.
A free account's cookie additionally unlocks `gemini-3.1-pro`, the `-thinking` variants of 3.6 Flash / 3.5 Flash-Lite / 3.1 Pro,
image / video input, image generation, music and canvas.
Paid accounts only: `gemini-3.8-flash`, `gemini-4-argon` (and their `-thinking` variants; free accounts are
downgraded to 3.5 Flash-Lite) and video generation (`gemini-video` / `/v1/videos`).

### Why does every message create a new conversation on gemini.google.com?

Each request is its own conversation by default. Enable **server-side multi-turn** (`multi_turn`) on the panel's
Settings page and consecutive messages of one conversation reuse the same web session, sending only the newest
turn while history stays server-side. To stop conversations piling up in the web app, you have two options:
enable **temporary chat** (`temporary_chat`) so the conversation never enters the list at all (cleaner, not used
for training, nothing to delete — recommended), or enable **auto-delete web conversation**
(`auto_delete_conversation`) to delete it after each reply. Both only matter for signed-in accounts.

### Can I use it as the backend for Cursor / Dify / WorkBuddy-style agents?

You can connect them (it is OpenAI-compatible), and plain chat and coding work fine. But the web app caps a single
request at roughly **130,000 bytes** (about 160,000 with a cookie); agent tools that push tens of kilobytes of system
prompt and history per request will get off-topic answers and look "dumb" — a limit of the web model itself, not of
the proxy. Function calling is prompt-level, so the model does not always answer in the expected format.

### Is gemini-3.8-flash available?

Yes: `gemini-3.8-flash` and `gemini-3.8-flash-thinking`, for paid Gemini accounts; requests from free accounts are
downgraded server-side to 3.5 Flash-Lite. `gemini-3.7-flash` remains as an alias so older clients keep working.

### Can I use Gemini 4 Argon through this? How?

Yes, and without a Gemini API key. The model is `gemini-4-argon`; `gemini-4-argon-thinking` is the upstream "High"
thinking tier and carries `reasoning_content`; the Medium tier is `gemini-4-argon-thinking-medium` (see the model table).
There is one requirement: the account behind the cookie must show "4 Argon" in the model picker on gemini.google.com.
If it does, the account is eligible (Google AI Pro / Ultra tier; a Pro plan that came bundled or through a student offer
counts), so add its cookie in the admin dashboard and request `gemini-4-argon`.
Requests from a free account, or from one without Argon, are **silently downgraded to 3.5 Flash-Lite** upstream with no
error, which is why the model is not exposed when no cookie is configured and never runs anonymously. If you have a cookie
and still get downgraded, check the web app first for "4 Argon" on that account; if the browser has several accounts signed in,
also check that the entry's "account index" in the Cookie pool is right.

### Which model does image generation use? Can it output 2K?

`gemini-image` uses the web app's image-generation tool slot rather than a pinned model: whatever backend the web
app currently uses (e.g. Nano Banana 2) is what you get, automatically. By default you get about one
megapixel (e.g. 1376×768). On an Ultra account, turn on **Full-size image download** (`image_upscale`) in Settings to
get the 2× upscale, 2752×1536 (about 2K); other accounts don't get the upscale, but they get the original file instead
of the recompressed one.

### Why does streaming turn into a single response behind Dify / a reverse proxy?

This project streams for real (every upstream frame is forwarded as it arrives), including requests carrying `tools`
and `/v1/responses`; only a tool-call block is held until it is complete. Responses already carry `X-Accel-Buffering: no`; if output still
arrives in one piece behind Nginx or similar, check the reverse proxy's own buffering — connected directly, the
proxy streams chunk by chunk.

### Does it support MySQL / PostgreSQL?

Yes. Set `SQL_DSN` (`mysql://user:pass@host:3306/db` or `postgres://user:pass@host:5432/db?sslmode=disable`);
unset means SQLite. Tables are created automatically and all three share one schema. SQLite is the easy choice for a
single instance; MySQL / PostgreSQL only matter when several instances share one pool.

## Star History

If you read this far, it probably solves something for you. A ⭐ helps others find it; open an issue when something breaks, and send a PR when you have fixed it.

[![Star History Chart](https://api.star-history.com/svg?repos=zexadev/gemini-web2api-go&type=Date)](https://star-history.com/#zexadev/gemini-web2api-go&Date)

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

