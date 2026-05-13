---
name: api
description: Call the Auron API on the user's behalf. Use whenever the user asks to look up, create, update, or delete anything in Auron — agents, meetings, conversations, entities, signals, knowledge stores, teams, telephony, analytics, etc. Also use when an Auron-related question is best answered by hitting a live endpoint. Reads tokens from ~/.auron/config.json (require the auth skill first if missing).
disable-model-invocation: false
allowed-tools: Bash(${CLAUDE_SKILL_DIR}/bin/* *) Read(~/.auron/api-wiki/**) Read(~/.auron/openapi.json) Read(~/.auron/config.json)
---

# Auron API

A bundled CLI (`auron-api`) lets you discover, search, and call any endpoint in the Auron OpenAPI spec. The flow is:

1. **Sync** the spec into `~/.auron/openapi.json` and a per-tag wiki at `~/.auron/api-wiki/<tag>.md`.
2. **Search** for the endpoint relevant to the user's request.
3. **Load** only the wiki file(s) you need into context.
4. **Call** the endpoint with the token from `~/.auron/config.json`.

## Host info

- Wiki present: !`test -d ~/.auron/api-wiki && echo yes || echo no`
- Token present: !`test -f ~/.auron/config.json && echo yes || echo no`
- OS / arch: !`uname -s` / !`uname -m`

## Pick the right binary

Use the host symlink on Unix: `${CLAUDE_SKILL_DIR}/bin/auron-api`. On Windows pick `auron-api-windows-amd64.exe`. Full matrix:

| OS / arch     | Binary                          |
| :------------ | :------------------------------ |
| macOS arm64   | `auron-api-darwin-arm64`        |
| macOS amd64   | `auron-api-darwin-amd64`        |
| Linux amd64   | `auron-api-linux-amd64`         |
| Linux arm64   | `auron-api-linux-arm64`         |
| Windows amd64 | `auron-api-windows-amd64.exe`   |

## Step 1 — Ensure auth + spec

If `~/.auron/config.json` is missing or expired, stop and invoke the `auth` skill first. Never prompt the user for credentials yourself.

If `~/.auron/api-wiki/` is missing or older than 24h, run:

```bash
"${CLAUDE_SKILL_DIR}/bin/auron-api" sync
```

This writes `~/.auron/openapi.json` plus one markdown file per tag.

## Step 2 — Find the right endpoint

Run search against the user's intent — do not load wiki files blindly.

```bash
"${CLAUDE_SKILL_DIR}/bin/auron-api" search "schedule meeting with rep" --limit 10
```

Returns JSON with `method`, `path`, `operationId`, `summary`, `tags`, and the `wiki` filename. Pick the most plausible match. If unsure, search again with different terms or list sections:

```bash
"${CLAUDE_SKILL_DIR}/bin/auron-api" list
```

## Step 3 — Load only the wiki you need

Read the specific `~/.auron/api-wiki/<tag>.md` file with the Read tool. Don't read every file — pick 1–3 based on search results. Each tag file lists every operation with parameters, request-body shape, and response codes. For the full schema of a request/response body, read the relevant `components.schemas` block from `~/.auron/openapi.json`.

## Step 4 — Call the endpoint

```bash
"${CLAUDE_SKILL_DIR}/bin/auron-api" call GET /agents --query "limit=20"
"${CLAUDE_SKILL_DIR}/bin/auron-api" call POST /meetings --body @/tmp/payload.json
echo '{"title":"Discovery call"}' | "${CLAUDE_SKILL_DIR}/bin/auron-api" call POST /meetings --body -
```

Flags:

| Flag       | Form                            | Notes                                                  |
| :--------- | :------------------------------ | :----------------------------------------------------- |
| `--query`  | `--query "k=v,k2=v2"`           | Comma-separated key=value pairs                        |
| `--header` | `--header "X-Trace=abc"`        | Extra request headers                                  |
| `--body`   | `--body @file.json` or `-`      | File path (prefixed `@`) or stdin (`-`) or literal     |
| `--raw`    | `--raw`                         | Print response body verbatim (skip JSON pretty-print)  |

The CLI:
- Reads `Bearer` token from `~/.auron/config.json`. If the token's `expires_at` is past, it errors and tells you to re-auth.
- Resolves the base URL from `servers[0].url` in the synced openapi (currently `https://dev.useauron.ai/api/v1`), so the `path` you pass starts at `/`.
- Pretty-prints JSON to stdout, writes `METHOD URL → status` to stderr.
- Exits non-zero on HTTP ≥ 400.

## Rules

- **Auth is a prerequisite.** If `~/.auron/config.json` is missing or expired, run the `auth` skill — do not attempt the call.
- **Read params before calling.** Always check the wiki entry's parameter list before constructing a call; mandatory path/query params produce 4xx otherwise.
- **Mutations require confirmation.** Before any `POST`, `PUT`, `PATCH`, or `DELETE`, summarize what will happen (method, path, body) and ask the user to confirm. Reads are safe to run unprompted.
- **Don't echo tokens or full response bodies that may contain PII.** Summarize, and offer to show specific fields.
- **Don't loop.** If a call returns 4xx, read the error, fix the request, and try once more — then stop and ask the user.
- **Don't invent endpoints.** Only call paths that appear in `search` results or the wiki.
