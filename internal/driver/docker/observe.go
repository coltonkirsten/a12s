package docker

import (
	"context"
	"fmt"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/engine"
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
		name := s.Labels[engine.LabelComputer]
		for _, m := range s.Mounts {
			if m.Type == mount.TypeVolume {
				attached[m.Name] = append(attached[m.Name], name)
			}
		}
		res, err := d.cli.ContainerInspect(ctx, s.ID, client.ContainerInspectOptions{})
		if engine.IsNotFound(err) {
			continue // removed since the list
		}
		if err != nil {
			return obs, fmt.Errorf("observe: inspect %s: %w", name, err)
		}
		obs.Computers[name] = engine.ComputerState(name, d.NetworkName(name), res.Container)
	}
	slist, err := d.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: d.serviceFilter()})
	if err != nil {
		return obs, fmt.Errorf("observe: list services: %w", err)
	}
	for _, s := range slist.Items {
		name := s.Labels[engine.LabelService]
		for _, m := range s.Mounts {
			if m.Type == mount.TypeVolume {
				attached[m.Name] = append(attached[m.Name], name)
			}
		}
		res, err := d.cli.ContainerInspect(ctx, s.ID, client.ContainerInspectOptions{})
		if engine.IsNotFound(err) {
			continue
		}
		if err != nil {
			return obs, fmt.Errorf("observe: inspect service %s: %w", name, err)
		}
		obs.Services[name] = engine.ComputerState(name, d.NetworkName(name), res.Container)
	}

	vols, err := d.cli.VolumeList(ctx, client.VolumeListOptions{Filters: d.fleetFilter().Add("label", LabelDrive)})
	if err != nil {
		return obs, fmt.Errorf("observe: list volumes: %w", err)
	}
	for _, v := range vols.Items {
		id := v.Labels[LabelDrive]
		obs.Drives[id] = driver.DriveState{ID: id, Name: v.Name, AttachedTo: attached[v.Name], Computer: v.Labels[engine.LabelComputer], Path: v.Labels[LabelPath]}
	}
	return obs, nil
}

func (d *Driver) Watch(ctx context.Context) (<-chan driver.Event, error) {
	return d.eng.Events(ctx), nil
}
