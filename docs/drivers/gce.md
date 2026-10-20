# GCE driver

The GCE driver runs each agent computer as its own Compute Engine VM. The VM runs Container-Optimized OS (COS) and
the agent computer's container in its own Docker engine, with the same settings as the Docker driver; the VM is the
isolation boundary, with its own kernel. The Controller runs in a container on a VM in the same subnet, and Fleets
run on it unchanged, within the limits below.

Use it when agents need their own cloud identity, a sandbox stronger than a shared kernel, or their own size. For a
laptop or a single host, the Docker driver stays simpler, faster to wake and cheaper when agents stay awake.

## What differs from Docker

- **Wake takes tens of seconds**, not a second: a VM boots. See [Sleep and wake](#sleep-and-wake).
- **No Services** (`services:` is refused at admission) and **no `attach`** (there is no owner host to bind a folder
  from). `a12s shell` isn't available; talk to agent computers with `a12s send`.
- **Home drives have a hard size**: persistent disks, 10 GiB unless `resources` asks for more. They grow, never
  shrink.
- **Egress** is enforced by the VPC firewall plus rules inside each VM: an agent computer can reach only the
  Controller's egress proxy and mTLS listener, with no DNS, as on Docker.

## Setting up a project

Use a GCP project for the fleet alone. The Controller's service account can attach any service account in its
project to a VM (that is how per-computer identity works), so a project of its own keeps that power away from every
other account. It also gives the fleet its own quotas, audit log and billing line.

`hack/gce/bootstrap.sh <project> <fleet> [region] [zone] [controller-machine-type]` creates, idempotently:

| Resource | Name | What for |
|---|---|---|
| VPC and subnet | `a12s-<fleet>` (`10.20.0.0/24`) | Agent computers get no external address. |
| Static internal address | `a12s-<fleet>-controller` (`10.20.0.2`) | Agent computers reach the Controller by address. |
| Firewall rules | see [below](#network-and-firewall) | |
| Cloud Router and NAT | `a12s-<fleet>` | The Controller's way out (model APIs, allowed hosts). |
| Service account | `a12s-controller@<project>.iam.gserviceaccount.com` | The Controller's identity. |
| Custom role | `a12sController`, bound on the project | [Controller permissions](#controller-permissions) |
| Controller VM | `a12s-<fleet>-controller` (Debian with Docker, e2-small) | Runs the Controller and holds the fleet's images. |

Then, on the Controller VM (`gcloud compute ssh a12s-<fleet>-controller --tunnel-through-iap`): install a12s, write
`driver.yaml`, and `a12s up` as usual. The images the fleet uses are pulled to the Controller VM; each agent
computer's VM gets them from there, so agent computer VMs need no registry and no internet.

## Choosing the driver

Put `driver.yaml` next to `controller.json`, in `~/.a12s/<fleet>/` on the Controller VM:

```yaml
driver: gce
gce:
  subnetwork: a12s-lab          # the fleet's subnet, in the Controller VM's region (required)
  # zone: us-central1-a         # default: the Controller VM's zone
  # families: [e2, n2d, t2d]    # tried in order when a zone has no capacity; one architecture
  # defaultShape: e2-small      # for agent computers that set no resources
  # bootDiskGb: 10
  # snapshotPolicy: a12s-nightly
  # identity: per-computer      # default: none
```

Any command that starts the Controller reads it. Changing it replaces the Controller container; the fleet's state
stays. Without the file, the fleet runs on Docker. The project, zone, address and service account come from the
Controller VM itself. `driver.yaml` is not part of the Fleet: the Fleet changes while the Controller runs, the
substrate doesn't. There are no GCE fields in `fleet.yaml`.

## Network and firewall

The driver checks these rules when a Fleet is applied and refuses the Fleet, with a fix, if they are missing or
broader; it never edits firewalls. `bootstrap.sh` creates them:

| Rule | Direction | Targets | Allows |
|---|---|---|---|
| `a12s-<fleet>-computers-deny-egress` | egress, priority 1000 | tag `a12s-computer` | nothing (deny all to `0.0.0.0/0`) |
| `a12s-<fleet>-computers-to-controller` | egress, priority 900 | tag `a12s-computer` | tcp 3128 and 7443 to the Controller's `/32` only |
| `a12s-<fleet>-controller-to-computers` | ingress | tag `a12s-computer` | tcp 2376 (engine) and 7443 (computerd) from the Controller's `/32` only |
| `a12s-<fleet>-computers-to-controller-in` | ingress | tag `a12s-controller` | tcp 3128 and 7443 from tag `a12s-computer` |
| `a12s-<fleet>-iap-ssh` | ingress | tag `a12s-controller` | tcp 22 from IAP |

Two things the VPC firewall can't see are handled inside each VM at every boot: the metadata server (and its DNS)
is reachable from inside any VM, and a container can reach its own host. The container may reach only the
Controller's ports, plus the metadata server's HTTP port when the agent computer has its own identity; container
DNS and traffic to the host are dropped.

The Controller drives each VM's Docker engine over mutual TLS: the engine trusts only the Controller's client
certificate from the Fleet's CA, and the Controller pins the engine's certificate, which the VM made at its first
boot and published as a guest attribute (only the VM can write those; with per-computer identity the agent computer
reaches the metadata server and could overwrite it, which only makes its own wakes fail). Credentials reach an agent computer the same
way as on Docker, over that link into tmpfs; nothing secret is in instance metadata, which anyone who can read the
instance can read.

## Controller permissions

The `a12sController` role holds exactly what the driver calls:

- `compute.instances.{create,delete,get,list,start,stop,setMetadata,setLabels,setTags,setMachineType,attachDisk,detachDisk,setServiceAccount,getGuestAttributes}`
- `compute.disks.{create,get,list,delete,resize,use,setLabels}`, `compute.resourcePolicies.use`
- `compute.subnetworks.{get,use}`, `compute.zoneOperations.{get,list}`, `compute.firewalls.list`,
  `compute.machineTypes.list`
- `iam.serviceAccounts.{create,delete,get,actAs}` (used only with per-computer identity)

A project allows 100 user-managed service accounts by default; more agent computers with their own identity need a
quota increase.

## Identity: one service account per agent computer

With `identity: per-computer`, every agent computer gets its own Google service account, with no keys and no roles,
and its VM runs as it. Your app grants that account what it needs, for example `roles/storage.objectUser` on one
bucket; Cloud Audit Logs then name the agent for every Google API call it makes (turn on Data Access logs for the
services agents use). `a12s status` and the API's `agentComputers[].identity` show the account's email, which has a
hash suffix you can't guess. Google client libraries and `gcloud` inside the agent computer get tokens from the
metadata server, which only these agent computers can reach. The hosts they call still need to be in
`egress.allow` (for example `storage.googleapis.com`): identity says who the agent is, the allowlist where it may go.

Removing an agent computer deletes its account. A new account with the same email is a new principal, so an agent
computer created again under the same name starts with no access.

## Sleep and wake

Sleep stops the agent computer's container (with the same 25-second flush as on Docker), then its VM. A stopped VM
bills only for its disks. Wake starts the VM, waits for its engine, rebuilds the container if the agent computer
changed while it slept, and starts it: expect tens of seconds, not the second or so a container takes (the exit
gate measured wake-to-Ready p50 38 to 53 s, p95 45 to 59 s on e2-medium in us-central1-a). Messages wait
in the Controller's inbox meanwhile, so a sleeping agent computer loses nothing; it answers later. A change to a
sleeping agent computer never boots its VM: it applies at the next wake.

`power.sleepAfterIdle` picks the trade-off per agent computer:

| Mode | `sleepAfterIdle` | For |
|---|---|---|
| Sleep right away | `1s` | Rare messages (daily jobs). Cheapest; every message waits for a wake. |
| Warm for a while | for example `15m` or `2h` | Bursts: the first message wakes it, follow-ups find it warm. |
| Always warm | `off` | Chat-speed answers. The VM bills around the clock. |

Each start bills at least a minute, so an agent computer messaged more often than every few minutes should stay warm.

Machine shapes: an agent computer that sets `resources` gets the cheapest shape of the first family with at least
that much sustained CPU and memory (E2 custom shapes included; shared-core shapes count as their sustained CPU:
e2-micro 0.25, e2-small 0.5, e2-medium 1). One that sets none gets `defaultShape` (e2-small: 2 GiB, enough for one
agent's harness and computerd). e2-micro (`cpu: 250m, memory: 1Gi`) also works for light agents: in the exit gate a
Turn that clones a repository, searches it and runs `npm ci` peaked at 360 MB on it and took 17 s, against 200 MB and
11 s on e2-small, since its shared CPU throttles sooner. The VM is the limit; the container itself has none. When the zone has no capacity
for the shape, wake tries the same size in each next family (`families`, default `e2, n2d, t2d`); the next wake
starts from the first family again. If none has capacity, the wake fails and `a12s status` says so.

## When things fail

| Event | What happens | What `a12s status` shows |
|---|---|---|
| The agent computer's container dies (crash or out of memory) | The driver records why and stops the VM, so a dead agent computer doesn't bill. The Controller restarts it like on Docker. | `exited with code N` or `out of memory` |
| Host maintenance | E2 and N-series VMs live-migrate. | Nothing |
| Host error | GCE stops the VM (`automaticRestart` is off: a12s makes power decisions). The power policy wakes it when a message waits. | `host error` |
| The VM shuts itself down | Recorded like a host error. | `the VM shut itself down` |
| No capacity in the zone | Wake tries the next family; see above. | `zone Z has no capacity for SHAPE` |
| Someone else stops or deletes the VM | Reported to the Controller like a dead or removed container. | As on Docker |
| The VM's boot script fails | The wake fails at once with the failing line. | The error |
| The Controller restarts | Nothing is lost: everything it needs is on the instances' labels and the disks, and the staged changes on its state disk. | |
| The zone fails | Nothing automatic: home disks are zonal. Restore a snapshot into another zone, change `zone`, and `a12s up`. | |

## Running the exit gate

`hack/gce/gate.sh <project> <logfile> [region] [zone]` runs the exit gate on a real project made for it, from your
machine: it bootstraps the project (fleet `gate`, an e2-standard-4 Controller VM), ships the checkout to the
Controller VM, builds the images, bundles and `a12s` there, and runs `hack/exit-gate.sh` against the GCE driver with
per-computer identity on. Then it checks, logging one `PASS`/`FAIL` line each:

- wake-to-Ready p50 and p95 over 5 wakes, failing above `A12S_GCE_WAKE_P95` seconds (default 90);
- from inside an agent computer: the allowed host works through the proxy, while a public IP, DNS (even the
  metadata server's), its own VM's engine, the Docker bridge's gateway and another agent computer's VM don't;
- the agent's own service account, granted `roles/storage.admin` for the test, creates a bucket with a token from
  the metadata server, and Cloud Audit Logs name that account;
- a sizing measurement: memory, CPU time and image and volume bytes of an e2-micro and an e2-small agent computer,
  idle and through a Turn that clones a repository, searches it and installs npm packages.

It ends with `a12s down --purge` and `hack/gce/teardown.sh`, which deletes what bootstrap made (`A12S_GCE_KEEP=1`
keeps it). It needs `GH_TOKEN`, `~/.a12s/anthropic` and `~/.a12s/lab/openai.json` like `hack/exit-gate.sh`, and
`gcloud` logged in as a principal with, on the project: `roles/serviceusage.serviceUsageAdmin`,
`roles/compute.admin`, `roles/iam.serviceAccountAdmin`, `roles/iam.roleAdmin`,
`roles/resourcemanager.projectIamAdmin`, `roles/iam.serviceAccountUser`, `roles/iap.tunnelResourceAccessor`,
`roles/logging.viewer` and `roles/storage.admin` (or `roles/owner`). The project needs billing; a run costs cents
of VM minutes.

## Taking a fleet down

`a12s down` on the Controller VM deletes the agent computers' VMs (and their service accounts) and the Controller's
container, and keeps the home disks; `a12s up` reattaches them. `a12s down --purge` also deletes the home disks after
asking. The VPC, firewall rules, Controller VM and service account stay: delete the project to remove everything.
