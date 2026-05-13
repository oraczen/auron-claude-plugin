---
name: auth
description: Authenticate the user to Auron via OIDC. Use when the user says "log in to Auron", "sign in", "auth", "authenticate", when an Auron API call fails with 401/unauthorized, or before any skill that needs Auron API access. Runs a local callback server on http://lh.useauron.com:5872/callback, opens the browser for OIDC login, and writes tokens to ~/.auron/config.json.
disable-model-invocation: false
allowed-tools: Bash(${CLAUDE_SKILL_DIR}/bin/* *) Bash(uname *) Read(~/.auron/config.json)
---

# Auron authentication

Signs the user in to Auron using OIDC authorization-code + PKCE. A bundled CLI (`auron-auth`) opens the browser, captures the callback on `http://lh.useauron.com:5872/callback`, exchanges the code for tokens, and writes them to `~/.auron/config.json`.

## Host info

- OS: !`uname -s`
- Arch: !`uname -m`
- Existing config: !`test -f ~/.auron/config.json && echo "present" || echo "missing"`

## How to sign the user in

1. Pick the right binary from `${CLAUDE_SKILL_DIR}/bin/`. The symlink `auron-auth` points to the host binary on Unix; on Windows use `auron-auth-windows-amd64.exe` explicitly.

   | OS / arch        | Binary                                  |
   | :--------------- | :-------------------------------------- |
   | macOS arm64      | `auron-auth-darwin-arm64`               |
   | macOS amd64      | `auron-auth-darwin-amd64`               |
   | Linux amd64      | `auron-auth-linux-amd64`                |
   | Linux arm64      | `auron-auth-linux-arm64`                |
   | Windows amd64    | `auron-auth-windows-amd64.exe`          |

2. Run it. The default OIDC discovery URL is `https://dev.useauron.ai/api/.well-known/openid-configuration`.

   ```bash
   "${CLAUDE_SKILL_DIR}/bin/auron-auth"
   ```

   Optional flags / env vars:

   | Flag              | Env var                | Default                                                                       |
   | :---------------- | :--------------------- | :---------------------------------------------------------------------------- |
   | `--client-id`     | `AURON_CLIENT_ID`      | `auron-cli`                                                                   |
   | `--discovery-url` | `AURON_DISCOVERY_URL`  | `https://dev.useauron.ai/api/.well-known/openid-configuration`               |
   | `--scopes`        | `AURON_SCOPES`         | `openid profile email offline_access`                                         |
   | `--config`        | `AURON_CONFIG`         | `~/.auron/config.json`                                                        |
   | `--timeout`       | —                      | `5m`                                                                          |

3. The binary will:
   - fetch the discovery doc,
   - start a local server on `http://lh.useauron.com:5872/callback` (resolves to 127.0.0.1),
   - open the browser to the authorization endpoint with PKCE + a random `state`,
   - receive the code, exchange it for tokens at the token endpoint,
   - atomically write `access_token`, `refresh_token`, `id_token`, `expires_at`, `scope`, `issuer`, `client_id`, `obtained_at` to `~/.auron/config.json` with mode `0600`.

4. Confirm success by reading `~/.auron/config.json` and reporting the user's `scope` and `expires_at` back to them. Never print tokens.

## When auth fails

- **`invalid_client`** — the server requires client-secret auth (its `token_endpoint_auth_methods_supported` is `client_secret_basic`/`client_secret_post`). Stop and ask the user for a `client_secret` or a public client_id registered server-side.
- **Browser didn't open** — print the URL the binary logged; the user can paste it manually.
- **Port 5872 busy** — another process is bound. Ask the user to free it; the port is fixed because it must match the OIDC client's registered redirect URI.
- **Timeout** — re-run; the default wait is 5 minutes.

## Don't

- Don't read, log, or echo the token values.
- Don't write tokens anywhere other than the configured config path.
- Don't run this skill without the user asking (or without a clear 401 from an Auron API call).
