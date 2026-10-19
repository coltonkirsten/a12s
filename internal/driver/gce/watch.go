package gce

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/engine"
	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
)

// watchEvery paces the poll for VM state changes GCE makes or someone else makes: GCE pushes no instance events
// short of a log sink, and a 10-second poll is 6 reads a minute. Watch only speeds things up; Observe is the truth.
var watchEvery = 10 * time.Second

// watchStart is when Watch starts looking at operations; tests move it onto the fake's clock.
var watchStart = func() time.Time { return time.Now().UTC() }

// Watch merges two sources: the Docker events of each running VM's engine (a container's death or OOM), and
// operations on the fleet's instances that the driver didn't issue (a host error, a shutdown from inside the VM, a
// stop or delete by someone else).
func (d *Driver) Watch(ctx context.Context) (<-chan driver.Event, error) {
	w := &watcher{d: d, out: make(chan driver.Event), streams: map[string]bool{}, names: map[string]string{}, since: watchStart()}
	go w.run(ctx)
	return w.out, nil
}

type watcher struct {
	d   *Driver
	out chan driver.Event
	wg  sync.WaitGroup

	mu      sync.Mutex
	streams map[string]bool   // VM boots being streamed, by vmKey
	names   map[string]string // instance URL -> Computer
	gens    sync.Map          // Computer -> generation label
	since   time.Time
}

func vmKey(vm VM) string { return vm.Computer + "/" + strconv.FormatUint(vm.ID, 10) + "/" + vm.Started }

func (w *watcher) run(ctx context.Context) {
	defer func() {
		w.wg.Wait()
		close(w.out)
	}()
	for {
		w.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(watchEvery):
		}
	}
}

func (w *watcher) send(ctx context.Context, ev driver.Event) {
	select {
	case w.out <- ev:
	case <-ctx.Done():
	}
}

func (w *watcher) poll(ctx context.Context) {
	list, err := w.d.api.ListInstances(ctx, w.d.computerFilter())
	if err != nil {
		return
	}
	for i := range list {
		in := &list[i]
		computer := in.Labels[LabelComputer]
		w.mu.Lock()
		w.names[in.SelfLink] = computer
		w.mu.Unlock()
		w.gens.Store(computer, generationOf(in))
		if in.Status == "RUNNING" {
			w.stream(ctx, in)
		}
	}
	w.operations(ctx)
}

// stream follows one boot of a VM's engine until the stream ends (the VM stopped) or ctx does.
func (w *watcher) stream(ctx context.Context, in *gceapi.Instance) {
	vm := vmOf(in)
	key := vmKey(vm)
	w.mu.Lock()
	if w.streams[key] {
		w.mu.Unlock()
		return
	}
	w.streams[key] = true
	w.mu.Unlock()
	w.wg.Go(func() {
		defer func() {
			w.mu.Lock()
			delete(w.streams, key)
			w.mu.Unlock()
		}()
		gctx, cancel := context.WithTimeout(ctx, guestTimeout)
		g, err := w.d.guest(gctx, in)
		cancel()
		if err != nil {
			return
		}
		for ev := range g.Events(ctx) {
			if ev.Service || ev.Computer != vm.Computer {
				continue
			}
			switch ev.Type {
			case driver.EventOOM:
				_ = w.d.setFailure(ctx, vm.Computer, "oom")
			case driver.EventDied:
				w.died(ctx, vm.Computer, ev)
			default:
				continue // the driver itself replaces containers; a VM's removal comes from its operations
			}
			w.send(ctx, ev)
		}
	})
}

// died records why the container exited and stops its VM, so a dead agent computer doesn't bill. The Controller's
// own sleep stops the container too; the Computer's lock orders the two, and the VM is stopped by then.
func (w *watcher) died(ctx context.Context, computer string, ev driver.Event) {
	if !engine.IsStopExit(ev.ExitCode) {
		if in, err := w.d.instance(ctx, computer); err == nil && in.Labels[LabelFailure] != "oom" {
			_ = w.d.setFailure(ctx, computer, "exit-"+strconv.Itoa(ev.ExitCode))
		}
	}
	w.wg.Go(func() {
		defer w.d.lock(computer)()
		in, err := w.d.instance(ctx, computer)
		if err != nil || in.Status != "RUNNING" {
			return
		}
		g, err := w.d.guest(ctx, in)
		if err != nil {
			return
		}
		if st, exists, err := g.Container(ctx); err == nil && exists && !st.Running {
			_ = w.d.stopVM(ctx, in)
		}
	})
}

// operations turns GCE's and other principals' operations on the fleet's instances into events. The driver's own
// operations carry the Controller's account as their user.
func (w *watcher) operations(ctx context.Context) {
	w.mu.Lock()
	since := w.since
	w.mu.Unlock()
	ops, err := w.d.api.ListOperations(ctx, `insertTime > "`+since.Format(time.RFC3339Nano)+`"`)
	if err != nil {
		return
	}
	for _, op := range ops {
		if t, err := time.Parse(time.RFC3339Nano, op.InsertTime); err == nil {
			w.mu.Lock()
			if t.After(w.since) {
				w.since = t
			}
			w.mu.Unlock()
		}
		w.mu.Lock()
		computer, ours := w.names[op.TargetLink]
		w.mu.Unlock()
		if !ours || op.User == w.d.cfg.ControllerAccount {
			continue
		}
		gen, _ := w.gens.Load(computer)
		g, _ := gen.(int64)
		ev := driver.Event{Computer: computer, Generation: g}
		switch op.OperationType {
		case "compute.instances.hostError":
			_ = w.d.setFailure(ctx, computer, "host-error")
			ev.Type = driver.EventDied
		case "compute.instances.guestTerminate":
			_ = w.d.setFailure(ctx, computer, "guest-terminate")
			ev.Type = driver.EventDied
		case "compute.instances.preempted", "stop", "suspend":
			ev.Type = driver.EventDied
		case "delete":
			ev.Type = driver.EventRemoved
		default:
			continue
		}
		w.d.dropGuest(computer)
		w.send(ctx, ev)
	}
}
