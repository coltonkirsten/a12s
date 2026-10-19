# GCE driver (experimental)

> **Experimental.** The GCE driver passes its unit tests against a fake of the GCE API; it has not yet passed the
> exit gate on a real project. Don't run production fleets on it.

The GCE driver runs each agent computer as its own Compute Engine VM. The VM runs Container-Optimized OS and the
agent computer's container in its own Docker engine, with the same settings as the Docker driver; the VM is the
isolation boundary. The Controller runs on a VM in the same subnet.

## Choosing it

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
```

Any command that starts the Controller reads it. Changing it replaces the Controller container; the fleet's state
stays. Without the file, the fleet runs on Docker. It is not part of the Fleet: the Fleet changes while the Controller
runs, the substrate doesn't.

## Identity: one service account per agent computer

```yaml
gce:
  identity: per-computer   # default: none
```

With `per-computer`, every agent computer gets its own Google service account, with no keys and no roles, and its
VM runs as it. Your app grants that account what it needs, for example `roles/storage.objectUser` on one bucket;
Cloud Audit Logs then name the agent for every Google API call it makes. `a12s status` and the API's
`agentComputers[].identity` show the account's email, which has a hash suffix you can't guess. Google client
libraries and `gcloud` inside the agent computer get tokens from the metadata server, which only these agent
computers can reach. The hosts it calls still need to be in `egress.allow` (for example `storage.googleapis.com`).

Removing an agent computer deletes its account. A new account with the same email is a new principal, so an agent
computer created again under the same name starts with no access.
