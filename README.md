# fim2chat

A small proxy that lets [Zed](https://zed.dev)'s OpenAI-compatible **edit predictions** work with an endpoint that only offers `/v1/chat/completions`, such as a corporate OpenAI gateway (Portkey, Azure OpenAI).

Zed sends legacy completion requests to `/v1/completions`, which chat-only gateways don't serve. fim2chat accepts both fill-in-the-middle (FIM) insertion prompts and edit-rewrite prompts, turns them into chat requests, and converts the answer back.

```
Zed -> fim2chat POST /v1/completions -> corporate gateway POST /v1/chat/completions -> OpenAI
```

## Start with Docker

```bash
cp .env.example .env        # then edit: CORP_API_URL, CORP_API_KEY, CORP_API_MODEL
docker compose up -d --build
curl http://127.0.0.1:8787/health
```

Without Compose:

```bash
docker build -t fim2chat .
docker run -d --name fim2chat --restart unless-stopped \
  --env-file .env -p 127.0.0.1:8787:8787 fim2chat
```

- Publish the port on `127.0.0.1`. The proxy holds your API key, so anyone who can reach it can spend your quota.
- Behind a TLS-inspecting proxy, `docker build` can fail at `go mod download` with `x509: certificate signed by unknown authority`. Pass the root CA as a build secret (it is not stored in the image): `docker build --secret id=cacert,src=certs/corp-root.pem -t fim2chat .`. `docker compose build` has no flag for this; build with `docker build` instead, or declare a `secrets` entry under `build:` in `docker-compose.yaml`.
- The image has the public CA bundle. If your gateway or network (e.g. Zscaler) uses a private root CA, put its PEM in `./certs/` and set `FIM_CA_CERT=/certs/corp-root.pem`. Compose mounts `./certs` for you. With `docker run`, add `-v "$PWD/certs:/certs:ro"`.

## Start without Docker

Requires Go 1.27.4 or newer.

```bash
go build -trimpath -ldflags="-s -w" -o fim2chat ./cmd/fim2chat
cp .env.example .env        # edit it; it is loaded from the working directory
./fim2chat
```

Or use flags only:

```bash
export CORP_API_KEY="<gateway key>"
./fim2chat -upstream https://<gateway>/v1/chat/completions -model gpt-6-luna \
  -max-tokens-field max_completion_tokens -extra-body '{"reasoning_effort":"none"}'
```

## Point Zed at it

In Zed's `settings.json`:

```json
{
  "edit_predictions": {
    "provider": "open_ai_compatible_api",
    "open_ai_compatible_api": {
      "api_url": "http://127.0.0.1:8787/v1/completions",
      "model": "your-model",
      "prompt_format": "qwen",
      "max_output_tokens": 128
    }
  }
}
```

Supported FIM `prompt_format` values: `qwen`, `codegemma`, `star_coder`, `deepseek_coder`, `code_llama`, `codestral`, `glm`. The edit formats `zeta` and `zeta2` rewrite an editable region; `sweep` rewrites the whole window around the cursor.

### Edit formats

Keep the provider, `api_url` and `model` from the settings above. Set these fields inside `edit_predictions.open_ai_compatible_api` for the format you want:

`zeta`:

```json
{"prompt_format": "zeta", "max_output_tokens": 256}
```

`zeta2`:

```json
{"prompt_format": "zeta2", "max_output_tokens": 256}
```

`sweep`:

```json
{"prompt_format": "sweep", "max_output_tokens": 1024}
```

fim2chat detects the format from the prompt; no fim2chat setting is needed. `zeta2_1` and other marker-based Seed-Coder variants are unsupported and return HTTP 400.

Raise `max_output_tokens` enough to fit the full replacement. Zed multiplies this setting by four for `zeta` and `zeta2`, but not for `sweep`; a sweep window of about 21 lines needs roughly 1024 tokens, depending on the code. A general chat model rewrites the region or window on each keystroke, which is slower and heavier than FIM. Use the fastest model available with reasoning off.

Edit formats use their own built-in rewrite instructions; `-system-prompt-file` applies only to FIM. Cleanup removes wrappers and cursor markers and preserves the region's trailing line endings. Empty, whitespace-only or truncated (`finish_reason=length`) answers produce a safe no-change reply: the original region for `zeta`, or an empty prediction for `zeta2` and `sweep`. `-raw` disables cleanup and sends the model text unchanged, but the no-change safety net for truncated or empty/whitespace-only answers still applies.

## Configuration

Every setting is a command-line flag and an environment variable (also settable in `.env`). Precedence: **flag > environment > `.env` > default**. `./fim2chat -h` lists all flags with their variables. `.env.example` documents each one.

**Upstream**

| Flag | Env var | Default | Meaning |
|---|---|---|---|
| `-upstream` | `CORP_API_URL` | *required* | Full URL of the chat endpoint, e.g. `https://host/v1/chat/completions` |
| `-api-key` | `CORP_API_KEY` | | Upstream key. If empty, the key Zed sends is forwarded |
| `-model` | `CORP_API_MODEL` | Zed's value | Model name sent upstream, overriding Zed's |
| `-auth-header` | `FIM_AUTH_HEADER` | `Authorization` | `Authorization` sends `Bearer <key>`; any other name (`api-key`, `x-portkey-api-key`) sends the raw key in that header |
| `-header` | `FIM_HEADERS` | | Extra upstream header `Name: value`. Repeat the flag, or separate several with `;` in the variable |
| `-timeout` | `FIM_TIMEOUT` | `30s` | Upstream timeout; on expiry the client gets `504` |
| `-ca-cert` | `FIM_CA_CERT` | | PEM file with extra root CAs (corporate CA) |
| `-insecure` | `FIM_INSECURE` | `false` | Skip TLS verification. Last resort only |

**Request shaping**

| Flag | Env var | Default | Meaning |
|---|---|---|---|
| `-max-tokens-field` | `FIM_MAX_TOKENS_FIELD` | `max_tokens` | Use `max_completion_tokens` for GPT-5/6 and o-series models |
| `-max-tokens` | `FIM_MAX_TOKENS` | `256` | Used when Zed sends no limit |
| `-temperature` | `FIM_TEMPERATURE` | `0` | Negative omits the field (needed by some reasoning models) |
| `-extra-body` | `FIM_EXTRA_BODY` | | JSON merged into the upstream body last, overriding other fields, e.g. `{"reasoning_effort":"none"}` |
| `-send-stop` | `FIM_SEND_STOP` | `false` | Also send Zed's stop sequences upstream (at most 4). They are always applied locally |
| `-system-prompt-file` | `FIM_SYSTEM_PROMPT_FILE` | | Replace the built-in FIM system prompt; edit formats keep their rewrite instructions |
| `-raw` | `FIM_RAW` | `false` | Turn off output clean-up; edit replies still use the no-change safety net for truncated or blank answers |

**Server and safety**

| Flag | Env var | Default | Meaning |
|---|---|---|---|
| `-listen` | `FIM_LISTEN` | `127.0.0.1:8787` | Address to listen on |
| `-auth-token` | `FIM_AUTH_TOKEN` | | Shared secret the editor must send as `Authorization: Bearer <token>` (put it in Zed's API key field). Needs `-api-key` |
| `-allowed-hosts` | `FIM_ALLOWED_HOSTS` | | Extra names accepted in the `Host` header besides `localhost` and IPs. `*` disables the check |
| `-max-body-bytes` | `FIM_MAX_BODY_BYTES` | `1048576` | Largest request from the editor; larger gets `413` |
| `-max-response-bytes` | `FIM_MAX_RESPONSE_BYTES` | `4194304` | Largest upstream response; larger gets `502` |
| `-log-level` | `FIM_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. `debug` logs request and response bodies, **which contain your source code** |
| `-log-format` | `FIM_LOG_FORMAT` | `text` | `text` or `json` |

Always on, with no setting needed: requests from web pages (any `Origin` or `Sec-Fetch-Site` header) and requests whose `Host` is not `localhost` or an IP are refused with `403`. This stops a browser tab from spending your key through CSRF or DNS rebinding. Set `-auth-token` if other users or machines can reach the port. Redirects from the upstream are never followed, so the key can't be sent to another host.

**Model tips.** Predictions fire on almost every keystroke, so use the fastest model you have, with reasoning off. For GPT-6/GPT-5 "Luna" models:

```bash
-max-tokens-field max_completion_tokens -extra-body '{"reasoning_effort":"none"}'
```

If predictions come back empty with `finish_reason=length`, reasoning used up the token budget. Turn reasoning off or raise the limit.

**Portkey.** Either put the provider in the model name (`-model "@<provider-slug>/<model>"`), or use explicit headers: `-auth-header x-portkey-api-key -header "x-portkey-provider: @<provider-slug>"`.

## How it works

1. **Parse.** fim2chat first checks for an anchored `zeta`, `zeta2` or `sweep` edit prompt and extracts its context and replacement region or window. Otherwise it parses the FIM markers, e.g. `<|fim_prefix|>…<|fim_suffix|>…<|fim_middle|>`, into context, prefix (code before the cursor) and suffix (code after it).
2. **Ask.** For FIM, it sends the file with a `<CURSOR>` marker and asks for only the text to insert. For edits, it sends recent edits and context with instructions to return the full rewritten region or window, keeping unchanged lines verbatim. Sweep has no cursor marker; the model predicts the next edit from the difference between the original and current windows.
3. **Clean.** Both paths strip code fences and `<think>` blocks and apply stop sequences. FIM also drops a re-typed copy of the current line and text repeated from the suffix unless it closes something the answer opened. Rewrite cleanup removes Zed wrappers and cursor markers, then restores the region's trailing line endings; truncated or unusable answers become no-change replies. `-raw` skips cleanup, while keeping the rewrite safety net.
4. **Reply.** It returns a standard `text_completion`. If Zed asked for `stream: true`, the answer is sent as one SSE chunk, because the upstream call itself is not streamed.

Every request gets a `request_id` that appears on all its log lines (and in the `X-Request-Id` response header), so one completion can be followed from Zed to the upstream and back. Secrets are redacted in logs.

**Layout:** `cmd/fim2chat` (entry point), `internal/config` (flags, env, `.env`), `internal/proxy` (HTTP handler and guard), `internal/fim` (prompt parsing and clean-up), `internal/upstream` (chat client), `internal/logging`, `internal/health` (Docker healthcheck). Run the tests with `go test -race ./...`.

**Limitations:** output quality depends on how well the model follows the insertion or rewrite instructions, and cleanup is heuristic. There is no content-based guard against a model that elides code in a nonempty rewrite (for example, replacing lines with a "rest unchanged" comment). Sweep strips one trailing newline that Zed appends to the current window; if the real window already ended in a newline at an empty final line of the file, the replacement loses that one trailing newline. Only the first prompt is used if Zed sends an array.
