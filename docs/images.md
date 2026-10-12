# Images and harness bundles

## Image tiers

There are two tiers, plus custom image references (`image: ghcr.io/you/your-image:tag`).

| Tier | Contents |
|---|---|
| `slim` (`a12s/slim:dev`) | `node:24-bookworm-slim` plus ca-certificates, git, ripgrep, python3, curl, util-linux, and computerd. |
| `browser` (`a12s/browser:dev`) | `slim` plus Debian Chromium, fonts, the Playwright library and the Playwright MCP server, and a seccomp profile so Chromium keeps its own sandbox. |

## Image references

A source build (`a12s version` says `dev`) uses local images: `a12s/slim:dev`, `a12s/browser:dev`,
`a12s/controller:dev`, `a12s/bundle-<kind>:<version>`. A release build resolves the images published with it:
`ghcr.io/coltonkirsten/a12s/<slim|browser|controller|svc-*>:<release>` and
`ghcr.io/coltonkirsten/a12s/bundle-<kind>:<harness version>`. `make release` pushes all of them,
for linux/amd64 and linux/arm64, before it publishes the binaries. `a12s up` pulls missing images on the host with
the docker CLI. The a12s images are public; a private registry of your own works through your `docker login`.

## Custom images

Toolchain-heavy images should be derived from a tier rather than a new tier: `FROM ghcr.io/coltonkirsten/a12s/slim:<release>`
(or `a12s/slim:dev` in a source build), then add your tools. Admission accepts a custom image only if it carries the
`ai.a12s.computerd.version` label (set on the tiers and inherited by every image built FROM them) from a compatible
release (same major, and same minor while 0.x; a `dev` build on either side only warns) and has computerd at
`/opt/a12s/computerd/dist/main.js`; otherwise `a12s up` changes nothing and says how to fix it. a12s starts
computerd as root itself, so an image's `ENTRYPOINT`, `CMD`, `USER` and `HEALTHCHECK` are ignored. A worked example
is in [Extending a12s](extending.md#a-custom-agent-image).

Build them with `make images` (slim, browser and controller).

## Harness bundles

The harness is never baked into an image or installed onto the home drive. A **HarnessBundle** is an OCI image
whose filesystem is `/bundle/{bundle.json, bin/<binary>, …}`:

```json
{"kind":"claude-code","version":"2.1.280","arch":"arm64","bin":"bin/claude","signInHosts":["api.anthropic.com", "…"]}
```

- The Controller extracts the bundle into the read-only cache volume at
  `/opt/a12s/bundles/<kind>-<version>-<imageID[:12]>/`, keyed by image identity.
- `signInHosts` are added to that Computer's egress allowlist automatically, so a Fleet lists only the hosts its work
  needs.
- Default refs are `a12s/bundle-<kind>:<version>` in a source build and
  `ghcr.io/coltonkirsten/a12s/bundle-<kind>:<version>` in a release; `fleet.lock` pins the image ID.
- Bumping a harness is one edit to `harness.version` and an `a12s up`. No image is rebuilt.

`make bundles` builds the Claude Code and Codex bundles locally. `.github/workflows/bundles.yml`, started by hand,
builds the latest upstream releases for amd64 and arm64, runs `images/bundles/conformance.sh` (which checks the adapter's
protocol and the receipt signal the inbox relies on), and publishes `ghcr.io/coltonkirsten/a12s/bundle-<kind>:<version>`.

## The browser tier

The agent drives the browser through Playwright MCP (navigate, click, type, read the page as an accessibility
snapshot, screenshot), which works the same for Claude Code and Codex. Browser traffic goes through the egress proxy
like everything else. The profile lives on the home drive (`~/.cache/ms-playwright/mcp-…`), so it survives sleep.

computerd launches Playwright MCP as a stdio server, as the agent (uid 1000), in `/home/agent`:

```text
command: node
args:    /opt/a12s/playwright-mcp/node_modules/@playwright/mcp/cli.js
         --config /opt/a12s/playwright-mcp/config.json
         --proxy-server $A12S_PROXY
```

`config.json` sets `executablePath: /usr/bin/chromium`, `headless: true` and `chromiumSandbox: true`. Scripts the
agent writes can `require("playwright")` (`NODE_PATH` is set) and must launch with
`executablePath: "/usr/bin/chromium", chromiumSandbox: true`.

### Chromium sandbox and seccomp

`images/browser/seccomp.json` is Docker's default seccomp profile plus one rule allowing `clone`, `unshare`, `setns`
and `chroot`, so Chromium's namespace sandbox works without `CAP_SYS_ADMIN` and without `--no-sandbox`. Without it
Chromium fails with "No usable sandbox!".

Linux hosts that restrict unprivileged user namespaces with AppArmor (Ubuntu 23.10+, including GitHub runners) also
need:

```sh
sudo sysctl kernel.apparmor_restrict_unprivileged_userns=0
```

Docker Desktop doesn't.

To check the sandbox by hand:

```sh
docker run --rm --user 1000:1000 --cap-drop ALL --security-opt no-new-privileges \
  --security-opt seccomp=images/browser/seccomp.json --read-only --tmpfs /tmp \
  --tmpfs /home/agent:uid=1000,gid=1000 -e HOME=/home/agent -w /home/agent \
  --network none -v "$PWD/images/browser/test:/test:ro" --entrypoint node \
  a12s/browser:dev /test/render.mjs     # or /test/mcp.mjs (via Playwright MCP)
```

`render.mjs` prints the page title and heading from a `data:` URL, a screenshot size, and `chrome://sandbox`, which
should report the namespace and seccomp-BPF sandboxes as active. `make check-full` runs the same checks.
