# Credentials

A Fleet names credentials (`credentials: [anthropic, github]`); the [bindings file](fleet-file.md#bindings-file)
maps each name to a file or environment variable on your machine. Values never go into an image, the Fleet file, a
container's config or a drive.

## What to bind

| Name | Value | How the harness gets it |
|---|---|---|
| `anthropic` | **Recommended:** a `claude setup-token` token (`sk-ant-oat…`) | `CLAUDE_CODE_OAUTH_TOKEN`. Long-lived, never refreshed inside the Computer, no credential file. |
| `anthropic` | An API key (`sk-ant-api…`) | `ANTHROPIC_API_KEY` |
| `anthropic` | A JSON credentials file | Symlinked as `$CLAUDE_CONFIG_DIR/.credentials.json` |
| `openai` | An OpenAI API key | `OPENAI_API_KEY` |
| `openai` | A copy of `~/.codex/auth.json` (ChatGPT sign-in) | Symlinked as `$CODEX_HOME/auth.json` |
| `github` | A GitHub token | `GH_TOKEN` and `GITHUB_TOKEN` |
| anything else | Any value | A file at `/run/a12s/secrets/<name>` only |

```sh
claude setup-token                 # sign in in the browser, copy the token
a12s creds set anthropic           # paste it; stored at ~/.a12s/<fleet>/anthropic (0600) and bound
```

Bind a setup token or an API key, not a copy of your interactive Claude login.

## How secrets are handled

- The CLI reads the bindings on your machine and sends the values to the Controller with `a12s up` and `a12s wake`.
  The Controller keeps them **in memory only**. After a Controller restart, `a12s status` says credentials aren't
  loaded and `a12s up` sends them again.
- The Controller writes each secret into the running container over `docker exec` stdin, into a tmpfs at
  `/run/a12s/secrets/<name>` (owned by the agent user, 0600). Nothing appears in `docker inspect`, argv, the
  container's environment, the image or any drive. Sleep clears them; wake injects them again.
- computerd passes each secret only to the harness process.
- If a harness refreshes a JSON login (Codex `auth.json`), it writes through the symlink onto tmpfs. computerd also
  moves back onto tmpfs any credential file a harness replaced on the home drive, after every Turn and at shutdown.
- **Refreshes are one-way.** A token refreshed inside a Computer is not written back to your machine. To rotate,
  update the binding and run `a12s up`. A setup token is never refreshed, so this only affects JSON logins.
- Credential rotation restarts the harness process, so a long-lived Codex server doesn't keep a revoked key.
  computerd reports the credential generation it has actually loaded (`a12s status`).

Services receive their credentials the same way, at `/run/a12s/secrets/<name>` owned by the Service's user. A Service
that needs an interactive sign-in uses [`a12s service login`](services.md#credentials-and-the-one-time-login).
