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
