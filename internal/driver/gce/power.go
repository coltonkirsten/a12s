package gce

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
)

// stockoutReasons are GCE's codes for a zone without capacity for a shape.
var stockoutReasons = []string{"ZONE_RESOURCE_POOL_EXHAUSTED", "ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS", "RESOURCE_POOL_EXHAUSTED"}

// start tries the first family's shape, then each fallback family's with the same CPU and memory: the spec fixes
// only those, so the family is the driver's choice. A stopped VM can change shape. It never moves zones by itself,
// since its home disk is zonal. Each wake starts from the first family, so a fallback lasts one wake.
func (d *Driver) start(ctx context.Context, in *gceapi.Instance, spec driver.ComputerSpec) error {
	var tried []string
	var last error
	current := lastSegment(in.MachineType)
	for _, fam := range d.cfg.Families {
		shape, err := d.shapeFor(ctx, spec, fam)
		if err != nil {
			continue // no shape that size in this family
		}
		if shape != current {
			if err := d.api.SetMachineType(ctx, in.Name, shape); err != nil {
				return fmt.Errorf("set machine type %s: %w", shape, err)
			}
			current = shape
		}
		err = d.api.StartInstance(ctx, in.Name)
		if err == nil {
			return nil
		}
		if !gceapi.HasReason(err, stockoutReasons...) {
			return fmt.Errorf("start VM: %w", err)
		}
		tried, last = append(tried, shape), err
	}
	if last == nil {
		return fmt.Errorf("no machine family of %s has a shape for %d millicpu and %d MiB", strings.Join(d.cfg.Families, ", "), spec.CPUMillis, spec.Memory/mib)
	}
	if ferr := d.setFailure(ctx, spec.Name, "no-capacity"); ferr != nil {
		last = errors.Join(last, ferr)
	}
	return fmt.Errorf("zone %s has no capacity for %s: %w", d.api.Zone, strings.Join(tried, ", "), last)
}

// setFailure records why a Computer stopped on its own, or clears it ("").
func (d *Driver) setFailure(ctx context.Context, computer, failure string) error {
	for range 5 {
		in, err := d.instance(ctx, computer)
		if err != nil {
			return err
		}
		if in.Labels[LabelFailure] == failure {
			return nil
		}
		labels := maps.Clone(in.Labels)
		if failure == "" {
			delete(labels, LabelFailure)
		} else {
			labels[LabelFailure] = failure
		}
		err = d.api.SetLabels(ctx, in.Name, labels, in.LabelFingerprint)
		if !gceapi.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("record why %s stopped: the instance's labels kept changing underneath", computer)
}

func failureText(label, zone, machineType string) (failure string, exit int, oom bool) {
	switch {
	case label == "":
		return "", 0, false
	case label == "oom":
		return "out of memory", 137, true
	case strings.HasPrefix(label, "exit-"):
		n, _ := strconv.Atoi(strings.TrimPrefix(label, "exit-"))
		return "exited with code " + strconv.Itoa(n), n, false
	case label == "host-error":
		return "host error", 0, false
	case label == "guest-terminate":
		return "the VM shut itself down", 0, false
	case label == "no-capacity":
		return "zone " + zone + " has no capacity for " + machineType, 0, false
	}
	return label, 0, false
}
