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
```

Build everything with `make`; see `PLAN.md` for layout and `EXIT-GATE.md` for
the acceptance run. MIT licensed.
