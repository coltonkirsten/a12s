# Images and harness bundles

## The slim image

Every agent computer runs `slim` or an image built FROM it (`image: ghcr.io/you/your-image:tag`).

| Image | Contents |
|---|---|
| `slim` (`a12s/slim:dev`) | `node:24-bookworm-slim` plus ca-certificates, git, ripgrep, python3, curl, util-linux, and computerd. |

## Image references

A source build (`a12s version` says `dev`) uses local images: `a12s/slim:dev`, `a12s/controller:dev`,
`a12s/bundle-<kind>:<version>`. A release build resolves the images published with it:
`ghcr.io/coltonkirsten/a12s/<slim|controller>:<release>` and
`ghcr.io/coltonkirsten/a12s/bundle-<kind>:<harness version>`. The release workflow (`.github/workflows/release.yml`, on a `v*` tag) pushes all of them, for linux/amd64 and
linux/arm64, before it publishes the binaries. `a12s up` pulls missing images on the host with
the docker CLI. The a12s images are public; a private registry of your own works through your `docker login`.

## Custom images

Add tools by deriving an image: `FROM ghcr.io/coltonkirsten/a12s/slim:<release>` (or `a12s/slim:dev` in a source
build), then install what the agent needs. Admission accepts a custom image only if it carries the
`ai.a12s.computerd.version` label (set on slim and inherited by every image built FROM it) from a compatible
release (same major, and same minor while 0.x; a `dev` build on either side only warns) and has computerd at
`/opt/a12s/computerd/dist/main.js`; otherwise `a12s up` changes nothing and says how to fix it. a12s starts
computerd as root itself, so an image's `ENTRYPOINT`, `CMD`, `USER` and `HEALTHCHECK` are ignored. A worked example
is in [Extending a12s](extending.md#a-custom-agent-image).

An image never changes the sandbox it runs in. When software in it needs syscalls Docker's default seccomp profile
blocks, the Fleet's owner sets [`security.seccomp`](fleet-file.md#agent-computer-fields) on that agent computer; a label or
file in the image can't.

Build the a12s images with `make images` (slim and controller).

### A browser in your image

a12s ships no browser. To give an agent one, build an image FROM slim with Chromium (and Playwright or the
Playwright MCP server, if the agent should drive it through MCP). Chromium's own sandbox needs user-namespace
syscalls (`clone`/`unshare` with `CLONE_NEWUSER`, `setns`, `chroot`) that Docker's default seccomp profile blocks;
without them it fails with "No usable sandbox!". Don't fall back to `--no-sandbox`: give that agent computer a
profile that is Docker's default plus those syscalls.

```yaml
agent_computers:
  browser:
    image: ghcr.io/you/browser-agent:1      # FROM ghcr.io/coltonkirsten/a12s/slim:0.8.0 + Chromium
    security:
      seccomp: ./profiles/chromium.json     # relative to fleet.yaml
```

The cookbook's [browser-agent example](https://github.com/coltonkirsten/a12s-cookbook/tree/main/examples/browser-agent)
has the Dockerfile, `profiles/chromium.json` and the Fleet. Browser traffic goes through the egress proxy like
everything else; a profile kept on the home drive survives sleep.

Linux hosts that restrict unprivileged user namespaces with AppArmor (Ubuntu 23.10+, including GitHub runners) also
need:

```sh
sudo sysctl kernel.apparmor_restrict_unprivileged_userns=0
```

Docker Desktop doesn't.

## Harness bundles

The harness is never baked into an image or installed onto the home drive. A **HarnessBundle** is an OCI image
whose filesystem is `/bundle/{bundle.json, bin/<binary>, …}`:

```json
{"kind":"claude-code","version":"2.1.280","arch":"arm64","bin":"bin/claude","signInHosts":["api.anthropic.com", "…"]}
```

- The Controller extracts the bundle into the read-only cache volume at
  `/opt/a12s/bundles/<kind>-<version>-<imageID[:12]>/`, keyed by image identity.
- `signInHosts` are added to that agent computer's egress allowlist automatically, so a Fleet lists only the hosts its work
  needs.
- Default refs are `a12s/bundle-<kind>:<version>` in a source build and
  `ghcr.io/coltonkirsten/a12s/bundle-<kind>:<version>` in a release; the Controller [pins](fleet-file.md#pins) the image ID.
- Bumping a harness is one edit to `harness.version` and an `a12s up`. No image is rebuilt.

`make bundles` builds the Claude Code and Codex bundles locally. `.github/workflows/bundles.yml` checks daily (or by hand) for a
Claude Code or Codex release that is not yet in ghcr; it stops there when there is none. For a new one it builds it for amd64 and arm64, runs `images/bundles/conformance.sh` (which checks the adapter's
protocol and the receipt signal the inbox relies on), and publishes `ghcr.io/coltonkirsten/a12s/bundle-<kind>:<version>`.
