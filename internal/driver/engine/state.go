package engine

import (
	"context"
	"strconv"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
)

// ComputerState reads a Computer's or Service's state from its container; Address is its IP on network.
func ComputerState(name, network string, c container.InspectResponse) driver.ComputerState {
	st := driver.ComputerState{
		Name:       name,
		Generation: ParseGeneration(c.Config.Labels),
		Power:      driver.PowerAsleep,
		InstanceID: c.ID,
		Network:    network,
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
		if ep := c.NetworkSettings.Networks[network]; ep != nil && ep.IPAddress.IsValid() {
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
		case s.Status == container.StateExited && !IsStopExit(s.ExitCode):
			st.Failure = "exited with code " + strconv.Itoa(s.ExitCode)
		}
	}
	return st
}

// IsStopExit: 143 is SIGTERM and 137 is SIGKILL after the stop timeout.
func IsStopExit(code int) bool { return code == 0 || code == 143 || code == 137 }

// Events streams the Fleet's Computer and Service deaths, OOMs and removals until ctx ends or the engine's stream
// fails, then closes the channel.
func (e *Engine) Events(ctx context.Context) <-chan driver.Event {
	// Label filters AND together, so the role is checked per event instead.
	filters := make(client.Filters).Add("label", LabelFleet+"="+e.fleet).
		Add("type", string(events.ContainerEventType)).
		Add("event", string(events.ActionDie), string(events.ActionOOM), string(events.ActionDestroy))
	res := e.cli.Events(ctx, client.EventsListOptions{Filters: filters})
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
				ev, ok := ToEvent(m)
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
	return out
}

func ToEvent(m events.Message) (driver.Event, bool) {
	attrs := m.Actor.Attributes
	ev := driver.Event{Computer: attrs[LabelComputer], Generation: ParseGeneration(attrs)}
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
