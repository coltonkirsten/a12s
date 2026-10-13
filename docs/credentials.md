# Credentials

A Fleet names credentials (`credentials: [anthropic, github]`); the [bindings file](fleet-file.md#bindings-file)
maps each name to a file or environment variable on your machine. Code can also set a credential on the running
Fleet ([below](#from-code-and-the-api)). Values never go into an image, the Fleet file, a container's config or a
drive; the Controller stores them in its own state volume.

## What to bind

| Name | Value | How the harness gets it |
|---|---|---|
| `anthropic` | **Recommended:** a `claude setup-token` token (`sk-ant-oat…`) | `CLAUDE_CODE_OAUTH_TOKEN`. Long-lived, never refreshed inside the agent computer, no credential file. |
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

- The CLI reads the bindings on your machine and sends the values to the Controller: `a12s up` sends every
  credential the bindings file binds, `a12s wake` the ones it needs, and `a12s creds set` uploads the one it sets
  when the Fleet's Controller is running.
- The Controller keeps the values in its state volume, in `credentials.json` (mode 0600), so they survive a
  Controller or Docker restart. Anyone who can read that file already has Docker access, and with it can exec into
  an agent computer and read `/run/a12s/secrets`; without it, every restart would break the Fleet's apps until the
  next `a12s up`. `a12s down` removes the state volume and the stored values with it. No API call returns a value.
- `a12s up --memory-only-credentials` starts a Controller that keeps them in memory only (the Controller's
  environment: `A12S_MEMORY_ONLY_CREDENTIALS=1`). The flag applies when that `up` starts a new Controller, so run
  `a12s down` first if one is running; on start it deletes a `credentials.json` left in the volume. After a restart
  of such a Controller, `a12s status` says credentials aren't loaded and `a12s up` sends them again.
- The Controller writes each secret into the running container over `docker exec` stdin, into a tmpfs at
  `/run/a12s/secrets/<name>` (owned by the agent user, 0600). Nothing appears in `docker inspect`, argv, the
  container's environment, the image or any drive. Sleep clears them; wake injects them again.
- computerd passes each secret only to the harness process.
- If a harness refreshes a JSON login (Codex `auth.json`), it writes through the symlink onto tmpfs. computerd also
  moves back onto tmpfs any credential file a harness replaced on the home drive, after every Turn and at shutdown.
- **Refreshes are one-way.** A token refreshed inside an agent computer is not written back to your machine. To rotate,
  `a12s creds set` it again (or update the binding and run `a12s up`). A setup token is never refreshed, so this
  only affects JSON logins.
- Credential rotation restarts the harness process, so a long-lived Codex server doesn't keep a revoked key.
  computerd reports the credential generation it has actually loaded (`a12s status`).

Services receive their credentials the same way, at `/run/a12s/secrets/<name>` owned by the Service's user. A Service
that needs an interactive sign-in uses [`a12s service login`](services.md#credentials-and-the-one-time-login).

## From code and the API

The SDKs and the API set, list and remove credentials on the running Fleet by name. These calls take the owner
token, or a [prefix token](security.md#tokens) for names that start with its prefix.

```ts
await fleet.credentials.set("stripe-test", process.env.STRIPE_KEY!);
for (const c of await fleet.credentials.list()) console.log(c.name, c.set, c.usedBy);
await fleet.credentials.remove("stripe-test");
```

```python
fleet.credentials.set("stripe-test", os.environ["STRIPE_KEY"])
for c in fleet.credentials.list():
    print(c.name, c.set, c.used_by)
fleet.credentials.remove("stripe-test")
```

- `set` stores the value in the Controller. Agent computers and Services that name the credential get the new value
  at once; one that was waiting for it starts.
- `list` names every credential the Fleet refers to or the Controller stores: `name`, `set` (whether it has a
  value) and `usedBy` (`used_by`), the agent computers and Services that name it. It never returns a value.
- `remove` deletes a stored value. While an agent computer or Service still names the credential it fails with
  `in_use` (409); remove the name from their `credentials` first. A name with no stored value is `not_found`.
- An agent computer created from code may name any credential the Controller holds, whether it was set from code or
  sent by `a12s up` from the bindings file.
- The order doesn't matter. An agent computer or Service that names a credential with no value yet is created anyway:
  the write warns (`agent_computers.job-1: credential dbname has no value yet; it starts when you set it (a12s creds
  set dbname)`) and it waits in `Provisioning` with `waiting: {credentials: [dbname], fix: "a12s creds set dbname"}`
  in its status and `agent_computer.phase` event. Setting the value (`creds set`, `credentials.set`, or `a12s up`
  sending it from the bindings file) starts it.
- `a12s up` adds and replaces values but never removes one it doesn't send, so a credential set from code stays until
  you remove it or run `a12s down`.

The routes: `GET /v1/credentials` → `{credentials: [{name, set, usedBy, updatedAt?}]}`; `PUT /v1/credentials/{name}`
`{"value": "…"}` → `{name, set, usedBy, updatedAt}`; `DELETE /v1/credentials/{name}` → 204. A name is lowercase
letters, digits, `-` and `_`. `updatedAt` is when the value last changed (`credentials-updated.json` next to the
values; absent for a value stored by an older Controller).
