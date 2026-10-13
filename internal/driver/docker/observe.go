package docker

import (
	"context"
	"fmt"
	"strconv"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
)

func (d *Driver) Observe(ctx context.Context) (driver.Observation, error) {
	obs := driver.Observation{
		Computers: map[string]driver.ComputerState{},
		Drives:    map[string]driver.DriveState{},
		Services:  map[string]driver.ComputerState{},
	}
	list, err := d.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: d.computerFilter()})
	if err != nil {
		return obs, fmt.Errorf("observe: list containers: %w", err)
	}
	attached := map[string][]string{} // volume name -> computers
	for _, s := range list.Items {
		name := s.Labels[LabelComputer]
		for _, m := range s.Mounts {
			if m.Type == mount.TypeVolume {
				attached[m.Name] = append(attached[m.Name], name)
			}
		}
		res, err := d.cli.ContainerInspect(ctx, s.ID, client.ContainerInspectOptions{})
		if isNotFound(err) {
			continue // removed since the list
		}
		if err != nil {
			return obs, fmt.Errorf("observe: inspect %s: %w", name, err)
		}
		obs.Computers[name] = d.computerState(name, res.Container)
	}
	slist, err := d.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: d.serviceFilter()})
	if err != nil {
		return obs, fmt.Errorf("observe: list services: %w", err)
	}
	for _, s := range slist.Items {
		name := s.Labels[LabelService]
		for _, m := range s.Mounts {
			if m.Type == mount.TypeVolume {
				attached[m.Name] = append(attached[m.Name], name)
			}
		}
		res, err := d.cli.ContainerInspect(ctx, s.ID, client.ContainerInspectOptions{})
		if isNotFound(err) {
			continue
		}
		if err != nil {
			return obs, fmt.Errorf("observe: inspect service %s: %w", name, err)
		}
		obs.Services[name] = d.computerState(name, res.Container)
	}

	vols, err := d.cli.VolumeList(ctx, client.VolumeListOptions{Filters: d.fleetFilter().Add("label", LabelDrive)})
	if err != nil {
		return obs, fmt.Errorf("observe: list volumes: %w", err)
	}
	for _, v := range vols.Items {
		id := v.Labels[LabelDrive]
		obs.Drives[id] = driver.DriveState{ID: id, Name: v.Name, AttachedTo: attached[v.Name], Computer: v.Labels[LabelComputer], Path: v.Labels[LabelPath]}
	}
	return obs, nil
}

func (d *Driver) computerState(name string, c container.InspectResponse) driver.ComputerState {
	st := driver.ComputerState{
		Name:       name,
		Generation: parseGeneration(c.Config.Labels),
		Power:      driver.PowerAsleep,
		InstanceID: c.ID,
		Network:    d.NetworkName(name),
	}
	s := c.State
	if s == nil {
		return st
	}
	if s.Running || s.Restarting || s.Paused {
		st.Power = driver.PowerAwake
	}
	st.Running = s.Running && !s.Paused && !s.Restarting
	if st.Running && c.NetworkSettings != nil {
		if ep := c.NetworkSettings.Networks[st.Network]; ep != nil && ep.IPAddress.IsValid() {
			st.Address = ep.IPAddress.String()
		}
	}
	if !st.Running {
		st.ExitCode, st.OOMKilled = s.ExitCode, s.OOMKilled
		switch {
		case s.OOMKilled:
			st.Failure = "out of memory"
		case s.Error != "":
			st.Failure = s.Error
		case s.Status == container.StateDead:
			st.Failure = "container is dead"
		case s.Status == container.StateExited && !isStopExit(s.ExitCode):
			st.Failure = "exited with code " + strconv.Itoa(s.ExitCode)
		}
	}
	return st
}

// isStopExit: 143 is SIGTERM and 137 is SIGKILL after the stop timeout.
func isStopExit(code int) bool { return code == 0 || code == 143 || code == 137 }

func (d *Driver) Watch(ctx context.Context) (<-chan driver.Event, error) {
	// Label filters AND together, so the role is checked per event instead.
	filters := d.fleetFilter().
		Add("type", string(events.ContainerEventType)).
		Add("event", string(events.ActionDie), string(events.ActionOOM), string(events.ActionDestroy))
	res := d.cli.Events(ctx, client.EventsListOptions{Filters: filters})
	out := make(chan driver.Event)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case <-res.Err:
				return
			case m := <-res.Messages:
				ev, ok := toEvent(m)
				if !ok {
					continue
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func toEvent(m events.Message) (driver.Event, bool) {
	attrs := m.Actor.Attributes
	ev := driver.Event{Computer: attrs[LabelComputer], Generation: parseGeneration(attrs)}
	switch attrs[LabelRole] {
	case RoleComputer:
	case RoleService:
		ev.Computer, ev.Service = attrs[LabelService], true
	default:
		return ev, false
	}
	switch m.Action {
	case events.ActionDie:
		ev.Type = driver.EventDied
		ev.ExitCode, _ = strconv.Atoi(attrs["exitCode"])
	case events.ActionOOM:
		ev.Type = driver.EventOOM
	case events.ActionDestroy:
		ev.Type = driver.EventRemoved
	default:
		return ev, false
	}
	return ev, ev.Computer != ""
}
