package gce

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/engine"
	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
)

// pollEvery paces waits for a VM to reach a state; tests shorten it.
var pollEvery = 2 * time.Second

// vmTimeout bounds a wait for one VM transition.
const vmTimeout = 5 * time.Minute

func (d *Driver) machineTypes(ctx context.Context) ([]gceapi.MachineType, error) {
	d.mu.Lock()
	types := d.types
	d.mu.Unlock()
	if types != nil {
		return types, nil
	}
	types, err := d.api.ListMachineTypes(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list machine types: %w", err)
	}
	d.mu.Lock()
	d.types = types
	d.mu.Unlock()
	return types, nil
}

// shapeFor is the shape in fam for spec. A spec that asks for nothing gets the default shape, or in a fallback
// family the smallest shape as large as it.
func (d *Driver) shapeFor(ctx context.Context, spec driver.ComputerSpec, fam string) (string, error) {
	types, err := d.machineTypes(ctx)
	if err != nil {
		return "", err
	}
	cpu, mem := spec.CPUMillis, spec.Memory
	if cpu == 0 && mem == 0 {
		if fam == family(d.cfg.DefaultShape) {
			return d.cfg.DefaultShape, nil
		}
		for _, t := range types {
			if t.Name == d.cfg.DefaultShape {
				cpu, mem = sustainedMillis(t), t.MemoryMb*mib
			}
		}
	}
	return PickShape(fam, cpu, mem, types)
}

func checkGCESpec(spec driver.ComputerSpec) error {
	if err := engine.CheckSpec(spec); err != nil {
		return err
	}
	if len(spec.Attach) > 0 {
		return errors.New("attach needs host folders, which the GCE driver doesn't have")
	}
	return nil
}

// StageComputer creates the VM the first time: it boots, materializes the container and stops again, so a bad image
// or bundle fails here. On an existing, stopped VM it records the new spec and resizes or reattaches; the container
// is rebuilt at the next wake, since booting a sleeping VM for every Fleet edit costs a boot each time.
func (d *Driver) StageComputer(ctx context.Context, spec driver.ComputerSpec) error {
	if err := checkGCESpec(spec); err != nil {
		return fmt.Errorf("stage %s: %w", spec.Name, err)
	}
	defer d.lock(spec.Name)()
	shape, err := d.shapeFor(ctx, spec, d.cfg.Families[0])
	if err != nil {
		return fmt.Errorf("stage %s: %w", spec.Name, err)
	}
	digest := specDigest(spec)
	for range 5 {
		in, err := d.instance(ctx, spec.Name)
		if errors.Is(err, driver.ErrNotFound) {
			return d.create(ctx, spec, shape, digest)
		}
		if err != nil {
			return fmt.Errorf("stage %s: %w", spec.Name, err)
		}
		if in.Status != "TERMINATED" {
			if err := d.stopIdle(ctx, in); err != nil {
				return fmt.Errorf("stage %s generation %d: %w", spec.Name, spec.Generation, err)
			}
			continue
		}
		staged := generationOf(in)
		if staged > spec.Generation {
			return fmt.Errorf("stage %s generation %d: generation %d is staged: %w", spec.Name, spec.Generation, staged, driver.ErrStaleGeneration)
		}
		if staged == spec.Generation && in.Labels[LabelSpec] == digest {
			if _, err := d.store.get(spec.Name, staged, digest); err == nil {
				return nil
			}
		}
		if err := d.store.put(spec); err != nil {
			return fmt.Errorf("stage %s: %w", spec.Name, err)
		}
		if err := d.restageVM(ctx, in, spec, shape); err != nil {
			return fmt.Errorf("stage %s: %w", spec.Name, err)
		}
		labels := maps.Clone(in.Labels)
		labels[LabelGeneration] = strconv.FormatInt(spec.Generation, 10)
		labels[LabelSpec] = digest
		err = d.api.SetLabels(ctx, in.Name, labels, in.LabelFingerprint)
		if gceapi.IsConflict(err) {
			continue // another writer moved the labels: read them again and re-check the generation
		}
		if err != nil {
			return fmt.Errorf("stage %s: %w", spec.Name, err)
		}
		return nil
	}
	return fmt.Errorf("stage %s: the instance's labels kept changing underneath", spec.Name)
}

// stopIdle stops a VM whose Computer isn't running (a stage interrupted before it stopped the VM, or a container
// that exited), so it can be staged. A running Computer is ErrRunning.
func (d *Driver) stopIdle(ctx context.Context, in *gceapi.Instance) error {
	var err error
	switch in.Status {
	case "STOPPING":
		_, err = d.waitStatus(ctx, in.Labels[LabelComputer], "TERMINATED")
		return err
	case "PROVISIONING", "STAGING":
		if in, err = d.waitStatus(ctx, in.Labels[LabelComputer], "RUNNING"); err != nil {
			return err
		}
	}
	if in.Status == "RUNNING" {
		g, err := d.guest(ctx, in)
		if err != nil {
			return fmt.Errorf("VM is running and its engine doesn't answer: %w", err)
		}
		if st, exists, err := g.Container(ctx); err != nil {
			return err
		} else if exists && st.Running {
			return fmt.Errorf("VM is running: %w", driver.ErrRunning)
		}
	}
	return d.stopVM(ctx, in)
}

// restageVM brings a stopped VM's shape and home disk to spec.
func (d *Driver) restageVM(ctx context.Context, in *gceapi.Instance, spec driver.ComputerSpec, shape string) error {
	if last(in.MachineType) != shape {
		if err := d.api.SetMachineType(ctx, in.Name, shape); err != nil {
			return fmt.Errorf("set machine type %s: %w", shape, err)
		}
	}
	home := d.api.ZoneURL("disks", d.diskName(spec.HomeDrive))
	for _, disk := range in.Disks {
		if disk.DeviceName == homeDevice {
			if disk.Source == home || last(disk.Source) == last(home) {
				return nil
			}
			if err := d.api.DetachDisk(ctx, in.Name, homeDevice); err != nil {
				return fmt.Errorf("detach home disk: %w", err)
			}
		}
	}
	if err := d.api.AttachDisk(ctx, in.Name, gceapi.AttachedDisk{DeviceName: homeDevice, Source: home, Mode: "READ_WRITE"}); err != nil {
		return fmt.Errorf("attach home disk: %w", err)
	}
	return nil
}

func (d *Driver) create(ctx context.Context, spec driver.ComputerSpec, shape, digest string) error {
	if err := d.store.put(spec); err != nil {
		return fmt.Errorf("stage %s: %w", spec.Name, err)
	}
	no := false
	in := &gceapi.Instance{
		Name:        d.instanceName(spec.Name),
		Description: "a12s agent computer " + d.fleet + "/" + spec.Name,
		MachineType: d.api.ZoneURL("machineTypes", shape),
		Labels: map[string]string{
			LabelFleet: d.fleet, LabelComputer: spec.Name, LabelRole: RoleComputer,
			LabelGeneration: strconv.FormatInt(spec.Generation, 10), LabelSpec: digest,
		},
		Metadata: &gceapi.Metadata{Items: d.cfg.BootMetadata},
		Disks: []gceapi.AttachedDisk{
			{Boot: true, AutoDelete: true, InitializeParams: &gceapi.InitializeParams{
				SourceImage: d.cfg.BootImage, DiskSizeGb: d.cfg.BootDiskGB, DiskType: d.api.ZoneURL("diskTypes", DiskType),
			}},
			{DeviceName: homeDevice, Source: d.api.ZoneURL("disks", d.diskName(spec.HomeDrive)), Mode: "READ_WRITE"},
		},
		// No access config: no external address.
		NetworkInterfaces: []gceapi.NetworkInterface{{Subnetwork: d.subnetLink}},
		// automaticRestart off: GCE never makes power decisions; a12s's power policy does.
		Scheduling:       &gceapi.Scheduling{AutomaticRestart: &no, OnHostMaintenance: "MIGRATE", ProvisioningModel: "STANDARD"},
		Tags:             &gceapi.Tags{Items: []string{NetworkTag}},
		ShieldedInstance: &gceapi.ShieldedInstance{EnableSecureBoot: true, EnableVtpm: true, EnableIntegrityMonitoring: true},
	}
	if err := d.api.InsertInstance(ctx, in); err != nil {
		return fmt.Errorf("stage %s: create VM: %w", spec.Name, err)
	}
	got, err := d.waitStatus(ctx, spec.Name, "RUNNING")
	if err != nil {
		return fmt.Errorf("stage %s: %w", spec.Name, err)
	}
	merr := d.materialize(ctx, got, spec)
	if err := d.stopVM(ctx, got); err != nil {
		return fmt.Errorf("stage %s: %w", spec.Name, errors.Join(merr, err))
	}
	if merr != nil {
		return fmt.Errorf("stage %s: %w", spec.Name, merr)
	}
	return nil
}

func (d *Driver) materialize(ctx context.Context, in *gceapi.Instance, spec driver.ComputerSpec) error {
	g, err := d.guest(ctx, in)
	if err != nil {
		return fmt.Errorf("reach the VM's engine: %w", err)
	}
	if err := g.Materialize(ctx, spec); err != nil {
		return err
	}
	return nil
}

// waitStatus polls until the VM is in one of want, or a state it can't leave on its own.
func (d *Driver) waitStatus(ctx context.Context, computer string, want ...string) (*gceapi.Instance, error) {
	ctx, cancel := context.WithTimeout(ctx, vmTimeout)
	defer cancel()
	for {
		in, err := d.instance(ctx, computer)
		if err != nil {
			return nil, err
		}
		for _, w := range want {
			if in.Status == w {
				return in, nil
			}
		}
		switch in.Status {
		case "TERMINATED", "SUSPENDED", "RUNNING":
			return nil, fmt.Errorf("VM is %s, not %s", strings.ToLower(in.Status), strings.ToLower(strings.Join(want, " or ")))
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("VM still %s: %w", strings.ToLower(in.Status), ctx.Err())
		case <-time.After(pollEvery):
		}
	}
}

// stopVM stops the container first, so the harness gets the same flush window it gets on Docker, then the VM.
func (d *Driver) stopVM(ctx context.Context, in *gceapi.Instance) error {
	if in.Status == "RUNNING" {
		if g, err := d.guest(ctx, in); err == nil {
			_ = g.Stop(ctx)
		}
	}
	d.dropGuest(in.Labels[LabelComputer])
	if err := d.api.StopInstance(ctx, in.Name); err != nil {
		return fmt.Errorf("stop VM: %w", err)
	}
	return nil
}

func (d *Driver) SetPower(ctx context.Context, computer string, power driver.Power, generation int64) error {
	defer d.lock(computer)()
	in, err := d.instance(ctx, computer)
	if errors.Is(err, driver.ErrNotFound) {
		return fmt.Errorf("power %s %s: %w", computer, power, driver.ErrNotStaged)
	}
	if err != nil {
		return fmt.Errorf("power %s %s: %w", computer, power, err)
	}
	switch staged := generationOf(in); {
	case generation < staged:
		return fmt.Errorf("power %s generation %d: generation %d is staged: %w", computer, generation, staged, driver.ErrStaleGeneration)
	case generation > staged:
		return fmt.Errorf("power %s generation %d: generation %d is staged: %w", computer, generation, staged, driver.ErrNotStaged)
	}
	switch power {
	case driver.PowerAwake:
		if err := d.wake(ctx, in, generation); err != nil {
			return fmt.Errorf("power %s awake: %w", computer, err)
		}
	case driver.PowerAsleep:
		if err := d.sleep(ctx, in); err != nil {
			return fmt.Errorf("power %s asleep: %w", computer, err)
		}
	default:
		return fmt.Errorf("power %s: unknown state %q", computer, power)
	}
	return nil
}

func (d *Driver) wake(ctx context.Context, in *gceapi.Instance, generation int64) error {
	computer := in.Labels[LabelComputer]
	spec, err := d.store.get(computer, generation, in.Labels[LabelSpec])
	if err != nil {
		return err
	}
	if in.Status == "STOPPING" {
		if in, err = d.waitStatus(ctx, computer, "TERMINATED"); err != nil {
			return err
		}
	}
	if in.Status == "TERMINATED" || in.Status == "SUSPENDED" {
		if err := d.start(ctx, in, spec); err != nil {
			return err
		}
	}
	if in, err = d.waitStatus(ctx, computer, "RUNNING"); err != nil {
		return err
	}
	g, err := d.guest(ctx, in)
	if err != nil {
		return d.abandonWake(ctx, in, fmt.Errorf("reach the VM's engine: %w", err))
	}
	if st, exists, err := g.Container(ctx); err == nil && exists && st.Running && st.Generation == generation {
		return nil
	}
	if err := g.Materialize(ctx, spec); err != nil {
		return d.abandonWake(ctx, in, err)
	}
	if err := d.callers.Require(ctx, computer); err != nil {
		return d.abandonWake(ctx, in, err)
	}
	if err := g.Start(ctx); err != nil {
		return d.abandonWake(ctx, in, err)
	}
	return nil
}

// abandonWake stops a VM that booted but can't run its Computer, so it doesn't bill while the Controller retries.
func (d *Driver) abandonWake(ctx context.Context, in *gceapi.Instance, cause error) error {
	if err := d.stopVM(context.WithoutCancel(ctx), in); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (d *Driver) start(ctx context.Context, in *gceapi.Instance, spec driver.ComputerSpec) error {
	shape, err := d.shapeFor(ctx, spec, d.cfg.Families[0])
	if err != nil {
		return err
	}
	if lastSegment(in.MachineType) != shape {
		if err := d.api.SetMachineType(ctx, in.Name, shape); err != nil {
			return fmt.Errorf("set machine type %s: %w", shape, err)
		}
	}
	if err := d.api.StartInstance(ctx, in.Name); err != nil {
		return fmt.Errorf("start VM: %w", err)
	}
	return nil
}

func lastSegment(u string) string { return last(u) }

func (d *Driver) sleep(ctx context.Context, in *gceapi.Instance) error {
	switch in.Status {
	case "TERMINATED", "SUSPENDED":
		return nil
	case "STOPPING":
		_, err := d.waitStatus(ctx, in.Labels[LabelComputer], "TERMINATED")
		return err
	case "PROVISIONING", "STAGING":
		var err error
		if in, err = d.waitStatus(ctx, in.Labels[LabelComputer], "RUNNING"); err != nil {
			return err
		}
	}
	return d.stopVM(ctx, in)
}

func (d *Driver) RemoveComputer(ctx context.Context, computer string) error {
	defer d.callers.Remove(computer)()
	defer d.lock(computer)()
	in, err := d.instance(ctx, computer)
	switch {
	case errors.Is(err, driver.ErrNotFound):
	case err != nil:
		return fmt.Errorf("remove %s: %w", computer, err)
	default:
		d.dropGuest(computer)
		// The home disk was attached with autoDelete off, so it outlives the VM.
		if err := d.api.DeleteInstance(ctx, in.Name); err != nil && !gceapi.IsNotFound(err) {
			return fmt.Errorf("remove %s: %w", computer, err)
		}
	}
	return d.store.remove(computer)
}

func (d *Driver) Observe(ctx context.Context) (driver.Observation, error) {
	obs := driver.Observation{
		Computers: map[string]driver.ComputerState{},
		Drives:    map[string]driver.DriveState{},
		Services:  map[string]driver.ComputerState{},
	}
	list, err := d.api.ListInstances(ctx, d.computerFilter())
	if err != nil {
		return obs, fmt.Errorf("observe: list instances: %w", err)
	}
	byLink := map[string]string{} // instance URL -> Computer
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range list {
		in := &list[i]
		name := in.Labels[LabelComputer]
		byLink[in.SelfLink] = name
		wg.Go(func() {
			st := d.vmState(ctx, in)
			mu.Lock()
			obs.Computers[name] = st
			mu.Unlock()
		})
	}
	wg.Wait()
	disks, err := d.api.ListDisks(ctx, "("+d.fleetFilter()+") (labels."+LabelDriveKind+"="+string(driver.DriveHome)+")")
	if err != nil {
		return obs, fmt.Errorf("observe: list disks: %w", err)
	}
	for _, disk := range disks {
		computer := disk.Labels[LabelComputer]
		id := driver.HomeDriveID(computer)
		st := driver.DriveState{ID: id, Name: disk.Name, Computer: computer}
		for _, u := range disk.Users {
			if n, ok := byLink[u]; ok {
				st.AttachedTo = append(st.AttachedTo, n)
			} else {
				st.AttachedTo = append(st.AttachedTo, last(u))
			}
		}
		obs.Drives[id] = st
	}
	return obs, nil
}

// guestTimeout bounds asking a running VM's engine about its container during Observe.
const guestTimeout = 5 * time.Second

// vmState is built from two layers. A stopped VM is asleep, with the failure its labels recorded. A running VM is
// what its container is; while it boots, before its engine answers, it is awake and not yet running.
func (d *Driver) vmState(ctx context.Context, in *gceapi.Instance) driver.ComputerState {
	name := in.Labels[LabelComputer]
	st := driver.ComputerState{
		Name:       name,
		Generation: generationOf(in),
		Power:      driver.PowerAsleep,
		InstanceID: strconv.FormatUint(in.ID, 10),
		Network:    d.cfg.Subnetwork,
	}
	switch in.Status {
	case "PROVISIONING", "STAGING":
		st.Power = driver.PowerAwake
		return st
	case "RUNNING":
	default:
		return st
	}
	gctx, cancel := context.WithTimeout(ctx, guestTimeout)
	defer cancel()
	g, err := d.guest(gctx, in)
	if err != nil {
		st.Power = driver.PowerAwake
		return st
	}
	cs, exists, err := g.Container(gctx)
	if err != nil {
		st.Power = driver.PowerAwake
		return st
	}
	if !exists {
		return st
	}
	cs.Name, cs.Generation, cs.InstanceID, cs.Network = name, st.Generation, st.InstanceID, st.Network
	cs.Address = ""
	if cs.Running {
		cs.Address = addressOf(in).String()
	}
	return cs
}

func (d *Driver) Watch(ctx context.Context) (<-chan driver.Event, error) {
	out := make(chan driver.Event)
	go func() {
		<-ctx.Done()
		close(out)
	}()
	return out, nil
}
