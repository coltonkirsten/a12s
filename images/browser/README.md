# browser tier

`a12s/slim:dev` + Debian `chromium` + fonts + `@playwright/mcp` (pinned in
`playwright-mcp/package-lock.json`) at `/opt/a12s/playwright-mcp`.

## Launching Playwright MCP (computerd)

Stdio MCP server, spawned by the harness as the agent (uid 1000), cwd `/home/agent`:

```
command: node
args:    /opt/a12s/playwright-mcp/node_modules/@playwright/mcp/cli.js
         --config /opt/a12s/playwright-mcp/config.json
         --proxy-server $A12S_PROXY
```

`config.json` sets `executablePath: /usr/bin/chromium`, `headless: true` and
`chromiumSandbox: true` (Playwright would otherwise pass `--no-sandbox`). The
profile defaults to `~/.cache/ms-playwright/mcp-…` on the home drive, so it survives sleep.
Agent-written scripts can `require("playwright")` (`NODE_PATH` is set) and must
launch with `executablePath: "/usr/bin/chromium", chromiumSandbox: true`.

## seccomp.json

Docker's default profile (moby `docker-v29.2.0`,
`vendor/github.com/moby/profiles/seccomp/default.json`) plus one rule allowing
`clone`, `unshare`, `setns` and `chroot` unconditionally, so Chromium's
namespace sandbox works without `CAP_SYS_ADMIN` and without `--no-sandbox`.
Without it Chromium fails with "No usable sandbox!". Linux hosts that restrict
unprivileged user namespaces with AppArmor (Ubuntu 23.10+, including GitHub
runners) also need `sysctl kernel.apparmor_restrict_unprivileged_userns=0`;
Docker Desktop does not.

## Verified (Docker 29.2, arm64, no network)

```sh
docker run --rm --user 1000:1000 --cap-drop ALL --security-opt no-new-privileges \
  --security-opt seccomp=images/browser/seccomp.json --read-only --tmpfs /tmp \
  --tmpfs /home/agent:uid=1000,gid=1000 -e HOME=/home/agent -w /home/agent \
  --network none -v "$PWD/images/browser/test:/test:ro" --entrypoint node \
  a12s/browser:dev /test/render.mjs     # or /test/mcp.mjs (via Playwright MCP)
```

`render.mjs` prints the page title/heading from a `data:` URL, a screenshot size,
and `chrome://sandbox` ("Namespace … PID/Network namespaces Yes … Seccomp-BPF
sandbox Yes … You are adequately sandboxed."). `mcp.mjs` calls `browser_navigate`
on a `data:` URL through the MCP server launched as above (minus the proxy flag).
