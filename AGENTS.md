# AGENTS.md

Guidance for AI coding agents working in this repository. Human-facing docs are in `README.md`.

## What this project is

`fim2chat` is a small Go proxy. It accepts Zed's legacy `POST /v1/completions` requests with fill-in-the-middle (FIM) insertion prompts or edit-rewrite prompts (`zeta`, `zeta2`, `sweep`), converts them to chat-completion requests for an upstream gateway (Portkey, Azure OpenAI, ...), and converts the answer back into a `text_completion` response. Most of the logic is in cleaning up the chat model's output for insertion at the cursor or replacement of an editable region/window. `zeta2_1` is unsupported and returns 400.

- Module: `ryakhovskiy/fim2chat`. Go 1.27.4 or newer. One external dependency (`github.com/joho/godotenv`); everything else is the standard library. There is no `vendor/` directory.
- Runs as a single static binary, or in a `FROM scratch` Docker image.

## Layout

| Path | Role |
|---|---|
| `cmd/fim2chat` | Entry point: loads config, wires packages, serves HTTP, handles shutdown |
| `internal/config` | Flags, environment variables, `.env` loading, precedence, validation |
| `internal/proxy` | `/v1/completions` handler, request/response shaping, the access guard (Host/Origin/token checks) |
| `internal/fim` | FIM and edit-rewrite prompt parsing, instructions and model-output clean-up (heuristic-heavy) |
| `internal/upstream` | HTTP client for the upstream chat endpoint |
| `internal/logging` | slog setup, request IDs, secret redaction, inbound middleware, logging `http.RoundTripper` |
| `internal/health` | `/health` probe used by the Docker `HEALTHCHECK` |

Dependency direction: `cmd` → `config` → `proxy`/`upstream`/`fim`/`logging`; `proxy` → `fim`, `upstream`, `logging`; `upstream` → `logging`. Keep it acyclic; do not make low-level packages import `config`.

## Commands

```bash
go build -trimpath -ldflags="-s -w" -o fim2chat ./cmd/fim2chat   # binary (git-ignored)
gofmt -l .                       # must print nothing
go vet ./...                     # must be clean
go test -race ./...              # must pass
go test -cover ./...             # coverage per package
go test ./internal/fim -run '^$' -fuzz FuzzClean -fuzztime 60s   # also FuzzParse, FuzzOverlaps
docker build -t fim2chat .       # behind a TLS-inspecting proxy add:
                                 #   --secret id=cacert,src=certs/corp-root.pem
```

Before reporting any change as done: `gofmt -l .`, `go vet ./...` and `go test -race ./...` all clean.

## Conventions

- **Config.** Every flag has an environment variable. Adding a setting means: register the flag (`internal/config/flags.go`), add it to `envOptions` (`internal/config/env.go`), validate it (`config.go`), add it to `.env.example`, and add it to the README tables. Flag > environment > `.env` > default.
- **Logging.** Use `logging.From(ctx)` (request-scoped, carries `request_id`) and `slog` key/value attributes. Message text is stable and lowercase; put data in attributes.
- **Errors.** Wrap with `%w`. Error messages must never include credentials, keys, tokens or request bodies.
- **Output clean-up** (`internal/fim/cleanup.go`, `internal/fim/editclean.go`) is heuristic. FIM clean-up must only ever remove text; rewrite clean-up may only remove text except for trailing-line-ending normalisation and the `NoChange()` fallback. Never split a multi-byte character or delete code the model needed. Every behavior change needs a table-test row and, if it touches parsing or overlap logic, a fuzz-invariant check.
- **Comments** explain why, not what. Keep exported identifiers documented.
- **Tests.** Table-driven. `internal/proxy` tests live in the external package `proxy_test` and use `httptest` for the upstream. Tests never touch the real network or a real key.
- **Dependencies.** Standard library first. Do not add a module without the lead's approval.

## Security rules (hard)

- Never read, print, copy or commit `.env`, `fim2chat.env`, `certs/*.pem`, or anything holding an API key. `.env.example` is the only tracked env file.
- Never log secrets. Redaction lives in `internal/logging/redact.go`; extend it rather than working around it.
- Upstream redirects are never followed (the key would leak to another host). Do not change that.
- `-log-level debug` logs request and response bodies, which contain user source code. Do not enable it on shared storage or paste such logs anywhere.
- Do not weaken the access guard (`internal/proxy/guard.go`) without an explicit task for it.

## Working as a team (Herdr)

When agents are started through Herdr, each takes one role. The role file is `.roles/<agent-name>.md`; **read yours completely before acting**.

| Agent name | File | Responsibility |
|---|---|---|
| `lead` | `.roles/lead.md` | Receives tasks, clarifies them with the user, plans, writes specs, coordinates, owns `PROGRESS.md` |
| `backend` | `.roles/backend.md` | Implements business logic in non-test code |
| `qa` | `.roles/qa.md` | Reviews code, writes and runs tests, reports bugs |
| `reviewer` | `.roles/reviewer.md` | Reviews structure, completeness and clarity; reports findings, never edits |

Shared rules:

- **File ownership avoids collisions.** `backend` edits non-test code and docs. `qa` edits `*_test.go` and test data only. `reviewer` edits nothing. `lead` edits only `PROGRESS.md`. If you need a change in a file you do not own, ask its owner.
- **No assumptions.** If something is unclear, ask: the user (via `lead`) or the owning agent. Do not guess and proceed.
- **Talk through the Herdr CLI** (`herdr agent prompt <name> "..." --wait`; check `herdr --help` and verify `HERDR_ENV=1` first). Use agent names, not guessed pane IDs. Do not close panes, tabs or workspaces you did not create.
- **Progress lives in `PROGRESS.md`** at the repo root. Only `lead` writes it; others report to `lead` and `lead` records it.
- **Specs are the contract.** `backend` implements the spec, `qa` tests against it, `reviewer` checks against it. A disagreement with the spec goes to `lead`, not into the code.
