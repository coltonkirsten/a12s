# a12s — agent computers

One file in git declares several agent computers. One command runs them. Prompt
any of them and watch the reply stream back. Put them to sleep and they cost
nothing; wake them and they pick up the same conversation.

Layer 1 (this repo) runs Claude Code and Codex computers on Docker. The design
doc is the source of truth: `docs/a12s-layer1-design.md`. `PLAN.md` maps the
code to it.

```text
a12s up                      # apply fleet.yaml: create, update or stage Computers
a12s prompt scout "hello"    # send a turn, stream its events
a12s sleep --all             # stop processes, keep disks
a12s wake --all              # resume happens on the next prompt
a12s status                  # power, session, warnings
a12s down                    # remove Computers and Controller, keep drives
```

## Quickstart (macOS, verified 2026-10-05)

Needs Go 1.26+, Docker Desktop running, and Claude Code signed in to your Claude account.

```sh
gh repo clone coltonkirsten/a12s && cd a12s
make build                     # go build; also writes bin/a12s
make images bundles            # slim, browser and controller images + Claude Code and Codex bundles (a few minutes the first time)

claude setup-token             # sign in in the browser; copy the sk-ant-oat… token it prints
mkdir -p ~/.a12s && pbpaste > ~/.a12s/anthropic && chmod 600 ~/.a12s/anthropic

cd examples/quickstart
cp a12s.bindings.yaml.example a12s.bindings.yaml   # anthropic -> ~/.a12s/anthropic
../../bin/a12s up                                  # starts the controller, creates "hello"
../../bin/a12s prompt hello "Reply with the single word pong."
../../bin/a12s sleep --all                         # stops it; the disk and session are kept
```

`prompt` prints `turn … started`, then `pong`, then `turn … completed`. An `[diagnostic] egress denied` line is
normal: Claude Code also tries telemetry hosts that the allowlist blocks.

Your Claude login reaches the Computer only through the bindings file: the token is injected into tmpfs at start and
never written to the image or the disk. For Codex on a ChatGPT login, bind `openai` to a copy of `~/.codex/auth.json`;
`examples/lab` shows a four-Computer Fleet with both. To delete the quickstart Fleet and its disk:

```sh
../../bin/a12s down --purge     # lists the drives and asks; plain `a12s down` keeps them
```

Build everything with `make`; see `PLAN.md` for layout and `EXIT-GATE.md` for
the acceptance run. MIT licensed.
