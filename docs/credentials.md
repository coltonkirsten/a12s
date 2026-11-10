# Credentials

A Fleet names credentials (`credentials: [anthropic, github]`); you set each value on the Fleet's Controller with
`a12s creds set <name>` or from code ([below](#from-code-and-the-api)), or the Fleet names a secret manager the
Controller reads it from itself ([From a secret manager](#from-a-secret-manager)). Values never go into an image, the
Fleet file, a container's config or a drive; the Controller stores them in its own state volume.

## What to set

| Name | Value | How the harness gets it |
|---|---|---|
| `anthropic` | **Recommended:** a `claude setup-token` token (`sk-ant-oat…`) | `CLAUDE_CODE_OAUTH_TOKEN`. Long-lived, never refreshed inside the agent computer, no credential file. |
| `anthropic` | An API key (`sk-ant-api…`) | `ANTHROPIC_API_KEY` |
| `anthropic` | A JSON credentials file | Symlinked as `$CLAUDE_CONFIG_DIR/.credentials.json` |
| `openai` | An OpenAI API key as `auth.json`: `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-…"}` | Symlinked as `$CODEX_HOME/auth.json` |
| `openai` | A copy of `~/.codex/auth.json` (ChatGPT sign-in) | Symlinked as `$CODEX_HOME/auth.json` |
| `openai` | A bare OpenAI API key | `OPENAI_API_KEY` only: Codex ignores it and fails with 401, so use the `auth.json` form |
| `github` | A GitHub token | `GH_TOKEN` and `GITHUB_TOKEN` |
| anything else | Any value | A file at `/run/a12s/secrets/<name>` only |

```sh
claude setup-token                 # sign in in the browser, copy the token
a12s creds set anthropic           # paste it; set on the Controller, stored nowhere else
```

Use a setup token or an API key, not a copy of your interactive Claude login.

## How secrets are handled

- `a12s creds set` reads the value from stdin and sets it on the Controller, starting the Controller if none runs.
  It is stored nowhere else on your machine; `a12s up` and `a12s wake` send no credentials.
- The Controller keeps the values in its state volume, in `credentials.json` (mode 0600), so they survive a
  Controller or Docker restart. Anyone who can read that file already has Docker access, and with it can exec into
  an agent computer and read `/run/a12s/secrets`; without it, every restart would break the Fleet's apps until every
  credential was set again. `a12s down` removes the state volume and the stored values with it. No API call returns
  a value.
- The Controller writes each secret into the running container over `docker exec` stdin, into a tmpfs at
  `/run/a12s/secrets/<name>` (owned by the agent user, 0600). Nothing appears in `docker inspect`, argv, the
  container's environment, the image or any drive. Sleep clears them; wake injects them again.
- computerd passes each secret only to the harness process.
- If a harness refreshes a JSON login (Codex `auth.json`), it writes through the symlink onto tmpfs. computerd also
  moves back onto tmpfs any credential file a harness replaced on the home drive, after every Turn and at shutdown.
- **Refreshes are one-way.** A token refreshed inside an agent computer is not written back to the Controller. To rotate,
  `a12s creds set` it again. A setup token is never refreshed, so this only affects JSON logins.
- Credential rotation restarts the harness process, so a long-lived Claude Code or Codex process doesn't keep a
  revoked key. Restarting it ends the session's [background work](concepts.md#background-work), so the restart
  waits until none runs. computerd reports the credential generation it has actually loaded (`a12s status`).

Services receive their credentials the same way, at `/run/a12s/secrets/<name>` owned by the Service's user.

## From a secret manager

The Fleet's top-level `credentials` names where the Controller reads a credential's value, so nobody pastes it into
the Controller and rotating it is one step in the secret manager:

```yaml
credentials:
  anthropic:
    gcpSecret: projects/my-project/secrets/fleet-anthropic   # Google Secret Manager; the latest version
agent_computers:
  lead:
    credentials: [anthropic]          # used as before
```

- **`gcpSecret`** is a Secret Manager secret, `projects/<project id or number>/secrets/<name>`. The Controller reads its
  `latest` version with the service account of the VM it runs on, from the metadata server: no key file. It needs a
  Controller on a GCE VM (any driver) whose service account has `roles/secretmanager.secretAccessor` on that secret.
  Surrounding whitespace, such as the newline `gcloud secrets versions add` keeps from a file, is trimmed. It is the
  only source today.
- The Controller reads every source when the Fleet names it and then **every minute**, and stores the value as if it
  had been set by hand: agent computers and Services that use it get a new value within a minute, with the same
  harness restart as [rotation](#how-secrets-are-handled), and wake and restart inject the stored value. A minute
  keeps a rotation quick for about 45,000 reads a month per secret.
- **To rotate**, add a new version to the secret. The new version is `latest`, so the old one is no longer read; disable
  or destroy it afterwards.
- **To withdraw a value**, disable or destroy the latest version (Secret Manager doesn't fall back to an older one), or
  delete the secret. At the next read the Controller drops the value, stops the running agent computers and Services
  that use it so none keeps the old value, and leaves them in Provisioning with
  `waiting: {credentials: [anthropic], fix: "add an enabled version to projects/…"}`. A new version starts them again.
- **A read that fails otherwise** (no permission, an outage, no metadata server) keeps the value in use; the
  credential's `sourceError` says why, and the Controller logs each new error once.
- A credential with a source can't be set or removed by hand: `a12s creds set`, `credentials.set`/`remove` and a
  `PUT /v1/fleet` that carries its value answer `409 conflict` with the fix. Remove it from the Fleet's `credentials` to
  set it by hand again; it keeps the value it had until then.
- No value is ever logged. The Controller logs which version it uses
  (`credential anthropic: value from projects/123/secrets/fleet-anthropic/versions/4`), and `GET /v1/credentials`
  shows it as `sourceVersion`.

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
- An agent computer created from code may name any credential the Controller holds, however it was set.
- The order doesn't matter. An agent computer or Service that names a credential with no value yet is created anyway:
  the write warns (`agent_computers.job-1: credential dbname has no value yet; it starts when you set it (a12s creds
  set dbname)`) and it waits in `Provisioning` with `waiting: {credentials: [dbname], fix: "a12s creds set dbname"}`
  in its status and `agent_computer.phase` event. Setting the value (`creds set` or `credentials.set`) starts it.
- A value stays until you remove it or run `a12s down`; `a12s up` never touches values.

The routes: `GET /v1/credentials` → `{credentials: [{name, set, usedBy, updatedAt?, source?, sourceVersion?,
sourceError?}]}` (`source` and the rest for a credential [from a secret manager](#from-a-secret-manager)); `PUT /v1/credentials/{name}`
`{"value": "…"}` → `{name, set, usedBy, updatedAt}`; `DELETE /v1/credentials/{name}` → 204. A name is lowercase
letters, digits, `-` and `_`. `updatedAt` is when the value last changed (`credentials-updated.json` next to the
values; absent for a value stored by an older Controller).
