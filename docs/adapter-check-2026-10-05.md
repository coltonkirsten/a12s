# Nexus 2 Layer 1: adapter check (2026-10-05)

Checks section 5 "Adapters" of `layer1-design.md` against the current official docs and the CLIs installed on mac-mini: **Claude Code 2.1.280** and **codex-cli 0.154.0**. The design doc was not edited.

## Verdict

| Harness | Planned | Verdict | Change |
|---|---|---|---|
| Claude Code | `claude -p` with stream-json in and out, `--session-id`/`--resume`, stop by SIGTERM then kill | **Right base, wrong stop.** | Use the **Claude Agent SDK** (TypeScript) as the adapter base and point it at the bundle's binary. Stop with SDK `interrupt()` first, then SIGTERM, then SIGKILL. |
| Codex | `codex app-server` JSON-RPC on stdio: `thread/start`/`thread/resume`, `turn/start`, `turn/interrupt` | **Still the best interface.** All four methods exist in 0.154.0. | Persist **all of CODEX_HOME**, not just `~/.codex/sessions`. Generate the protocol schema in CI for each bundle. |
| ACP | Third adapter, plug-in point | **Keep as third, not as a Claude/Codex path.** | Both ACP bridges are community-run. `session/load` (resume) is an optional capability, so the adapter must check for it. |

## Claude Code

**Better official interface: the Agent SDK.** Anthropic positions `claude -p` and the Python/TS SDKs as one product, the Agent SDK. The SDK is "a library that runs the Claude Code binary", so it is the same harness. It adds `interrupt()`, a `canUseTool` permission callback, hook callbacks, typed messages and session helpers. You only need raw `claude -p` if `computerd` isn't Node or Python. In that case you are re-implementing the SDK's stdin control protocol yourself.
- Pin the binary. The SDK bundles its own CLI, so set `pathToClaudeCodeExecutable` to the HarnessBundle binary or you will run two versions. We already hit this gotcha on RAVEN.
- Licensing: Anthropic doesn't let third-party products offer claude.ai login through the SDK. For a product, use API-key auth.

**Stop semantics (the main fix).** Per the docs, SIGTERM exits with code 143 and "leaves the turn that was in progress unfinished and records no result for it". On resume, that turn stays half-done unless `CLAUDE_CODE_RESUME_INTERRUPTED_TURN=1` is set. To end the turn cleanly, use SIGINT or SDK `interrupt()`. If SIGTERM lands while a permission prompt is waiting, the prompt is left unanswered. Closing input through the SDK cancels it. Recommended sequence for `turn.finished: interrupted`: `interrupt()`, wait for the result, then SIGTERM after a deadline, then SIGKILL. Check `system/init` `capabilities` (for example `interrupt_receipt_v1`) to detect features instead of comparing version strings.

**Sessions and resume after container replacement.**
- Transcripts live in `$CLAUDE_CONFIG_DIR/projects/<encoded-cwd>/<id>.jsonl`. Set `CLAUDE_CONFIG_DIR` onto the home drive.
- The directory name comes from the cwd. Keep the cwd path the same across generations, or set `CLAUDE_CODE_PROJECT_DIR_NAME`.
- Since 2.1.223, `--resume <id>` finds the ID in any project on the machine. `--resume` also accepts the absolute path to the `.jsonl`, which is the most robust option after a replacement.
- `--session-id <uuid>` assigns the ID for a new session, which matches `session: new`. `--fork-session` exists if you ever need a branch.
- Session files are "same machine only" unless you move the file or use the SDK `SessionStore`. The home-drive design covers this.

**Permissions.**
- `full` maps to `--permission-mode bypassPermissions` (or `auto`, which uses a classifier).
- `restricted` maps to `--permission-mode dontAsk` plus `--permission-prompts none`, which is new in 2.1.259 and present locally. Anything that would prompt is denied, Claude is told not to retry, and `AskUserQuestion` is removed. Each denial shows up as a `permission_denied` system message in stream-json and in `permission_denials` on the result, which maps directly onto the `permission` event.
- With the SDK, `canUseTool` gives a per-request decision point, which is better for auditing every request.
- Note: "a run where nothing sets a permission mode" can start in `auto`. Always pass the mode explicitly.

**Version churn to guard against.**
- `--bare` "will become the default for `-p` in a future release". Bare mode skips skills, hooks, MCP, CLAUDE.md and OAuth/keychain reads. That breaks the design's "agent writes skills and MCP config to the home drive" plan once it flips. Set `--setting-sources` and `--mcp-config` explicitly now, and add a conformance test that a home-drive skill and MCP server actually load.
- Without `--bare`, `-p` runs a repo's `.claude/settings.json` hooks and `.mcp.json` servers with no trust dialog. That is a prompt-injection route from cloned repos, so note it in the threat model.
- Feature flags arrive often (for example `--permission-prompts` in 2.1.259). The conformance test should assert the flags the adapter uses.

**Credential rotation.** `apiKeyHelper` is the documented hook "for dynamic or rotating credentials". Point it at a script that reads `/run/nexus/secrets`, and a rotated key should be picked up without a restart. Claude Code retries quietly on a 401/403 from a helper credential. Confirm the exact re-run cadence in a conformance test. Bare mode doesn't read `CLAUDE_CODE_OAUTH_TOKEN`. The restart-on-rotation rule can stay as the urgent-revocation path.

**restart_harness.** Fits cleanly: end the process, then relaunch with `--resume <id>` (or the SDK `resume`). Hooks, skills and MCP load fresh on start, but only when not in bare mode (see above). `SessionEnd` hooks run on exit.

## Codex

**Planned approach is correct.** OpenAI's app-server doc says it is for deep integration: auth, history, approvals and streamed events. The doc says "if you are automating jobs or running Codex in CI, use the Codex SDK instead". However, the Python Codex SDK itself "controls the local Codex app-server over JSON-RPC", so the SDK adds a wrapper and no new capability. `codex exec --json` has no interrupt or approval channel, so it is a poor fit. Keep app-server over stdio.

**Verified locally (schema from `codex app-server generate-json-schema`):**
- `thread/start`, `thread/resume` (requires `threadId`), `turn/start` (`threadId`, `input`), `turn/interrupt` (`threadId` and `turnId`, both required), plus `turn/steer` and `thread/fork`.
- Interrupt ends with `turn/completed` and `status: "interrupted"`, which maps cleanly onto `turn.finished`.
- Approval policy: `untrusted | on-request | never | {granular:{...}}`. Sandbox: `read-only | workspace-write | danger-full-access`. Both can be set per thread and per turn.
- The handshake is `initialize` then `initialized`. Set `clientInfo.name`. Some features need `capabilities.experimentalApi: true`.

**Permissions.**
- Approvals arrive as server-to-client requests: `item/commandExecution/requestApproval`, `item/fileChange/requestApproval`, `item/permissions/requestApproval`, `mcpServer/elicitation/request` and `item/tool/requestUserInput`. Legacy `execCommandApproval`/`applyPatchApproval` are still in the schema.
- The adapter must answer every one of them, or the turn hangs. Answer `decline` and emit a `permission` event.
- Suggested mapping:
  - `full`: `approvalPolicy: never` with `danger-full-access`, since the container is the sandbox.
  - `restricted`: `on-request` with `workspace-write`, and the adapter auto-declines.
- There is also an `approvalsReviewer` (automatic "guardian" review) option, worth noting but not needed.

**Gotchas.**
- **Session storage moved beyond `~/.codex/sessions`.** On 0.154.0, CODEX_HOME holds `state_5.sqlite`, `thread_history_1.sqlite`, `thread-writer-locks/` and a `migrate-rollouts` command for "paginated thread history". The design's lifecycle table lists only `~/.codex/sessions`. Set `CODEX_HOME` on the home drive and persist all of it. Test that a stale `thread-writer-locks` entry left by a killed container doesn't block `thread/resume`.
- The app-server is labelled **[experimental]** and the method list is large and growing. The bundle CI should regenerate the JSON schema each release and diff the methods the adapter uses.
- Don't use the new shared `codex app-server daemon` or `remote-control`. Use one stdio child per Computer.
- **Rotation without restart may be possible:** `account/login/start` accepts `{type:"apiKey", apiKey}` in-process. That is untested. Until it is proven, keep the design's restart-on-rotation, which is the safe default.
- **restart_harness:** a full restart plus `thread/resume` works. Codex also exposes `config/mcpServer/reload`, `skills/extraRoots/set` and a `skills/changed` notification, so a lighter reload may be possible later.

## ACP

- Core spec: `initialize`, `session/new`, `session/prompt`, the `session/update` stream, `session/cancel` (a notification), and `session/request_permission`. **`session/load` is optional**, behind the `loadSession` capability. The adapter must check for it, and emit `resume.failed` or refuse at admission when an agent lacks it.
- Claude via ACP means Zed's `claude-agent-acp`, which is built on the Agent SDK. Codex via ACP means `agentclientprotocol/codex-acp`. **Neither is official** from Anthropic or OpenAI, and each adds a lag layer behind the native protocol. That confirms the design: native first, ACP for custom harnesses only.

## Recommended edits for the next design revision (not applied)
1. Base the Claude adapter on the Agent SDK with the bundle binary pinned. Stop with `interrupt()`, then SIGTERM, then SIGKILL, not SIGTERM first.
2. Map `restricted` on Claude to `dontAsk` plus `--permission-prompts none`, and always pass a mode explicitly.
3. Put `CLAUDE_CONFIG_DIR` and the whole `CODEX_HOME` on the home drive. Keep the cwd stable or set `CLAUDE_CODE_PROJECT_DIR_NAME`.
4. Claude rotation via `apiKeyHelper` reading the tmpfs secret.
5. Conformance tests for: the bare-mode default flip, the flags the adapter uses, the Codex schema diff, resume after a kill (stale writer locks), and a home-drive skill or MCP loading after `restart_harness`.

## Sources
- Claude Code headless / Agent SDK CLI: https://code.claude.com/docs/en/headless
- Agent SDK overview: https://code.claude.com/docs/en/agent-sdk/overview
- Agent SDK sessions (storage, cross-host resume): https://code.claude.com/docs/en/agent-sdk/sessions
- Claude Code authentication (apiKeyHelper, precedence): https://code.claude.com/docs/en/authentication
- Codex app-server: https://learn.chatgpt.com/docs/app-server (was developers.openai.com/codex/app-server)
- Codex SDK: https://learn.chatgpt.com/docs/codex-sdk
- ACP protocol overview: https://agentclientprotocol.com/protocol/overview
- ACP agents list: https://agentclientprotocol.com/overview/agents
- Local: `claude --help` (2.1.280), `codex --help`, `codex app-server --help`, `codex app-server generate-json-schema` (0.154.0)
