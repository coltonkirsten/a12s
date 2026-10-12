# Fleet file reference

A Fleet is declared in three files that sit side by side:

| File | In git? | What it holds |
|---|---|---|
| `fleet.yaml` | yes | The portable desired state: Computers, drives, Services, grants. |
| `a12s.bindings.yaml` | **no** | Where each named credential comes from on this machine. |
| `fleet.lock` | yes | The exact image, bundle, preset and Service digests the Fleet runs. |

`a12s init` scaffolds all three (plus a `.gitignore` for the bindings file and a README).

The CLI finds the Fleet file from `--fleet`/`-F`, then `$A12S_FLEET`, then the nearest `fleet.yaml` in the current
directory or a parent. The bindings file is `a12s.bindings.yaml` next to it unless `--bindings` says otherwise.

## Example

```yaml
apiVersion: a12s.ai/v1alpha1
kind: Fleet
metadata: { name: lab }

defaults:
  resources: { cpu: 1, memory: 2Gi }
  home: { size: 10Gi }
  autonomy: full                     # full | restricted
  egress: { allow: [] }              # deny by default

drives:
  a12s-repo: { size: 20Gi, access: exclusive }
  reference: { size: 5Gi,  access: shared }

computers:
  scout:
    harness: { kind: claude-code, version: 2.1.280, model: claude-fable-5-1 }
    image: slim
    credentials: [anthropic]
    home: { preset: ./presets/scout }          # copied once, onto an empty home
    drives: [{ name: reference, at: /work/ref, readOnly: true }]
    egress: { allow: [api.anthropic.com] }
    power:
      sleepAfterIdle: 15m
      wakeOnMessage: { from: [local], maxWakesPerHour: 6 }

  builder:
    harness: { kind: codex, version: 0.154.0 }
    image: slim
    credentials: [openai, github]
    drives: [{ name: a12s-repo, at: /work/a12s }]
    egress: { allow: [api.openai.com, github.com, registry.npmjs.org] }
    grants: { send: [scout] }                  # builder may enqueue to scout's inbox
```

`examples/` has complete, runnable Fleets.

## Merging defaults

Every Computer inherits `defaults` and can override any field. **Maps merge** key by key; **lists replace**. So a
Computer's `egress.allow` replaces the default list instead of adding to it, and
`computers.x.inbox.maxMessages` overrides only that field of `defaults.inbox`. `defaults` apply to Computers only,
never to Services.

## Top level

| Field | Meaning |
|---|---|
| `apiVersion` | `a12s.ai/v1alpha1` |
| `kind` | `Fleet` |
| `metadata.name` | The Fleet name, a DNS label. Docker resources are named `a12s-<fleet>-…`. |
| `egress.enforcement` | `enforced` (default) or `unenforced`, for substrates that can't enforce egress. Printed on every status line. |
| `egress.allowPrivate` | CIDRs of private ranges that Services' `egress.tcp` may reach. Printed on every status line. |
| `defaults` | Any Computer field, applied to every Computer. |
| `drives` | Work drives, by name. |
| `computers` | Computers, by name (DNS labels). |
| `services` | Services, by name. See [Services](services.md). |

## Drives

```yaml
drives:
  <name>: { size: 20Gi, access: exclusive | shared | shared-write }
```

| `access` | Meaning |
|---|---|
| `exclusive` | One Computer attaches it, read-write. |
| `shared` | Any number attach it, all `readOnly: true`. |
| `shared-write` | Any number attach and write. Writers can coordinate with the `lock`/`release` system tools. |

On Docker, drives are named volumes. Docker can't enforce a quota on them, so `size` is recorded but not enforced.

## Computer fields

| Field | Meaning |
|---|---|
| `harness.kind` | `claude-code` or `codex`. |
| `harness.version` | The harness version; selects the bundle `<kind>@<version>`, pinned in `fleet.lock`. |
| `harness.model` | Optional model id passed to the harness. |
| `image` | `slim`, `browser`, or a custom image reference. See [Images](images.md). |
| `resources` | `{cpu, memory}`, e.g. `{ cpu: 2, memory: 4Gi }`. Zero means no limit. |
| `home.size` | Home drive size. |
| `home.preset` | A directory, relative to the Fleet file, copied once onto an empty home (skills, `CLAUDE.md`, settings). Its content hash is pinned in `fleet.lock`. |
| `autonomy` | `full` (default) or `restricted`. See [Permissions](concepts.md#permissions). |
| `credentials` | Credential names, resolved through the bindings file. See [Credentials](credentials.md). |
| `drives` | `[{name, at, readOnly?}]`: work drives and their absolute mount paths. |
| `egress.allow` | Hosts this Computer may reach over HTTPS. `["*"]` allows any public host on any port. The harness bundle's sign-in hosts are added automatically. |
| `inbox` | `{maxMessages, maxBytes, intro}`. See [Inbox](inbox.md#queue-size). |
| `power` | `{sleepAfterIdle, wakeOnMessage: {from, maxWakesPerHour}}`. See [Power policy](inbox.md#power-policy). |
| `uses` | Services this Computer may reach: `[db]` or `[{name: db, mcp: false}]`. See [Services](services.md#grants-and-reachability). |
| `grants.send` | Computers this one may enqueue to, through the `send` system tool. |

Power state is deliberately **not** in the file: sleep is an operation, not a definition. To create Computers
asleep, run `a12s up --asleep`.

Scaling is editing `computers:` and running `a12s up` again: only what changed is created, staged or removed.

## Bindings file

The Fleet names credentials; the bindings file says where each comes from on this machine, so the Fleet file stays
portable. Never commit it.

```yaml
credentials:
  anthropic: { file: ~/.a12s/anthropic }        # read from a file
  github:    { env: GH_TOKEN }                  # read from an environment variable
```

The CLI reads the bindings on the host and sends the values to the Controller with `up` and `wake`. `a12s creds set
<name>` reads a value from stdin, stores it at `~/.a12s/<fleet>/<name>` (0600) and binds it for you.

## fleet.lock

```yaml
a12s:     v0.1.0
images:   { slim: "sha256:…", browser: "sha256:…" }
bundles:  { "claude-code@2.1.280": "sha256:…", "codex@0.154.0": "sha256:…" }
presets:  { "./presets/scout": "sha256:…" }
services: { "db@a12s/svc-mcp-postgres:0.1": "sha256:…" }
```

`a12s: v0.1.0` pins the a12s release. A release binary of another version refuses to apply the Fleet and says how to
upgrade: install that version, or change the line to the new version and run `a12s up`. `a12s init` and the first
`a12s up` of a release binary write it; a source build only warns. Existing pins are never moved, so when you
upgrade, also delete the `slim`/`browser` entries under `images:` (they pin the old release's tiers) and rebuild any
custom image FROM the new base under a new tag.

`a12s up` resolves and appends missing entries and never changes an existing one. Moving a version forward is
explicit: change the version or ref in `fleet.yaml` (a new key), or run `a12s lock update services/<svc>` to re-pin
an unchanged Service ref.

## Validation

`a12s up` validates the whole Fleet before changing anything. Every error names the field and states the fix, for
example:

```text
computers.builder.inbox.maxBytes: 2Mi is above the 1Mi ceiling, which protects the model's context budget  fix: set 1Mi or less
scout uses db, but there is no Service db  fix: add services.db or remove it from scout.uses
```
