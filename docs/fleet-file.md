# Fleet file reference

`fleet.yaml` declares a Fleet: agent computers, drives, Services, grants. It is optional: the live Fleet is the source
of truth, and the file is one way to write it. `a12s init` scaffolds `fleet.yaml` and a README.

The CLI finds the Fleet file from `--fleet`/`-F`, then `$A12S_FLEET`, then the nearest `fleet.yaml` in the current
directory or a parent.

The SDK and the HTTP API change the same Fleet while it runs ([SDK](sdk.md#agent-computers)). `a12s up` makes the
running Fleet match the file again: it removes agent computers created from code and undoes changes made through the
API. Run `a12s export` first to keep them ([CLI](cli.md#keeping-changes-made-from-code)).

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

agent_computers:
  scout:
    harness: { kind: claude-code, version: 2.1.280, model: claude-fable-5-1 }
    image: slim
    credentials: [anthropic]
    home: { preset: ./presets/scout }          # files for its home drive; edit them and a12s up to update
    drives: [{ name: reference, at: /work/ref, readOnly: true }]
    egress: { allow: [api.anthropic.com] }
    power:
      sleepAfterIdle: 15m
      wakeOnMessage: { from: [local], maxWakesPerHour: 6 }

  builder:
    harness: { kind: codex, version: 0.154.0 }
    image: slim
    instructions: |                            # its AGENTS.md (CLAUDE.md for claude-code)
      You build and test changes to the a12s repo in /work/a12s.
    credentials: [openai, github]
    drives: [{ name: a12s-repo, at: /work/a12s }]
    egress: { allow: [api.openai.com, github.com, registry.npmjs.org] }
    grants: { send: [scout] }                  # builder may enqueue to scout's inbox
```

`examples/` has complete, runnable Fleets.

## Merging defaults

Every agent computer inherits `defaults` and can override any field. **Maps merge** key by key; **lists replace**.
So an agent computer's `egress.allow` replaces the default list instead of adding to it, and
`agent_computers.x.inbox.maxMessages` overrides only that field of `defaults.inbox`. `defaults` apply to agent
computers only, never to Services. Putting the harness, image and credentials in `defaults` keeps each agent computer
to its role, and lets code create one with only a name and instructions:

```yaml
defaults:
  harness: { kind: claude-code, version: 2.1.280, model: haiku }
  image: slim
  credentials: [anthropic]
agent_computers:
  lead:    { instructions: "You are lead. …", grants: { send: [worker1] } }
  worker1: { home: { preset: ./presets/worker }, grants: { send: [lead] } }
```

## Top level

| Field | Meaning |
|---|---|
| `apiVersion` | `a12s.ai/v1alpha1` |
| `kind` | `Fleet` |
| `metadata.name` | The Fleet name, a DNS label. Docker resources are named `a12s-<fleet>-…`. |
| `egress.enforcement` | `enforced` (default) or `unenforced`, for substrates that can't enforce egress. Printed on every status line. |
| `egress.allowPrivate` | CIDRs of private ranges that Services' `egress.tcp` may reach. Printed on every status line. |
| `defaults` | Any agent computer field, applied to every agent computer. |
| `drives` | Work drives, by name. |
| `agent_computers` | Agent computers, by name (DNS labels). |
| `services` | Services, by name. See [Services](services.md). |

## Drives

```yaml
drives:
  <name>: { size: 20Gi, access: exclusive | shared | shared-write }
```

| `access` | Meaning |
|---|---|
| `exclusive` | One agent computer attaches it, read-write. |
| `shared` | Any number attach it, all `readOnly: true`. |
| `shared-write` | Any number attach and write. Writers can coordinate with the `lock`/`release` system tools. |

On Docker, drives are named volumes. Docker can't enforce a quota on them, so `size` is recorded but not enforced.

## Agent computer fields

| Field | Meaning |
|---|---|
| `harness.kind` | `claude-code` or `codex`. |
| `harness.version` | The harness version; selects the bundle `<kind>@<version>` ([pinned](#pins)). |
| `harness.model` | Optional, passed to the harness as is: an alias it knows (`haiku`, `sonnet`, `opus` for Claude Code) or a full model id. Default: the harness's own default. |
| `image` | `slim`, or a custom image reference built FROM slim. See [Images](images.md#custom-images). |
| `instructions` | Text written to the home drive as `CLAUDE.md` (claude-code) or `AGENTS.md` (codex): the agent's standing role. |
| `files` | `{path: text}`: more text files for the home drive, by clean path relative to `/home/agent` (`.claude/skills/x/SKILL.md`, `.mcp.json`). A file that starts with `#!` is executable. Instructions and files together are at most 1 MiB. |
| `resources` | `{cpu, memory}`, e.g. `{ cpu: 2, memory: 4Gi }`. Zero means no limit. |
| `home.size` | Home drive size. |
| `home.preset` | A directory, relative to the Fleet file, of files for the home drive (`CLAUDE.md`, skills, settings). Only the Fleet file has it: `a12s up` reads the directory and sends its files as `files` (text files only; a symlink or a binary file is an error, and so is a path also in `files`). A preset `CLAUDE.md` (`AGENTS.md` for codex) together with `instructions` is an error: keep one. |
| `autonomy` | `full` (default) or `restricted`. See [Permissions](concepts.md#permissions). |
| `credentials` | Credential names; set each value with `a12s creds set <name>`. See [Credentials](credentials.md). |
| `drives` | `[{name, at, readOnly?}]`: work drives and their absolute mount paths. |
| `egress.allow` | Hosts this agent computer may reach over HTTPS. `["*"]` allows any public host on any port. The harness bundle's sign-in hosts are added automatically. |
| `security.seccomp` | A seccomp profile in Docker's JSON format that replaces Docker's default profile for this agent computer. In the Fleet file it is a path relative to the file (absolute paths are refused) or the profile itself as a mapping; `a12s up` reads the file and sends the profile. In the SDK and the HTTP API it is the profile's JSON object. Only the seccomp filter changes; see [Security: Seccomp](security.md#seccomp). Default: Docker's default profile. |
| `inbox` | `{maxMessages, maxBytes, intro}`. See [Inbox](inbox.md#queue-size). |
| `power` | `{sleepAfterIdle, wakeOnMessage: {from, maxWakesPerHour}}`; `sleepAfterIdle` is `off` or `1s` to `7d`. See [Power policy](inbox.md#power-policy). |
| `uses` | Services this agent computer may reach: `[db]` or `[{name: db, mcp: false}]`. See [Services](services.md#grants-and-reachability). |
| `grants.send` | Agent computers this one may enqueue to, through the `send` system tool. Each must exist in the Fleet. |

Power state is deliberately **not** in the file: sleep is an operation, not a definition. To have an agent computer
asleep, run `a12s sleep <name>` after `a12s up`.

Instructions and files are written onto the home drive before the agent computer first starts, and again when they
change: a changed file is rewritten at the next idle boundary and the session continues. Files the agent wrote
elsewhere on the home drive are kept; one of these files that the agent edited is overwritten.

Scaling is editing `agent_computers:` and running `a12s up` again: only what changed is created, staged or removed.

## Pins

The first time an image, bundle or Service ref is used, the Controller pins the image ID it resolves to, in its own
state. A pin never moves while its image exists, so to roll out a rebuilt image give it a new tag: a new ref is a new
pin. If the pinned image is gone, the ref's current image is used. `a12s service status <svc>` shows a Service's pin.
Presets and seccomp profiles are not pinned: edit one and run `a12s up`, and it applies at the next idle boundary.

To upgrade a12s, install the new release, rebuild custom images FROM the new `slim` under new tags, and run `a12s up`.

## Validation

`a12s up` validates the whole Fleet before changing anything. Every error names the field and states the fix, for
example:

```text
agent_computers.builder.inbox.maxBytes: 2Mi is above the 1Mi ceiling, which protects the model's context budget  fix: set 1Mi or less
scout uses db, but there is no Service db  fix: add services.db or remove it from scout.uses
```
