package gce

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
)

const mib = 1 << 20

// sharedCoreMillis is a shared-core shape's sustained CPU, which is what it is counted as, not its burst.
var sharedCoreMillis = map[string]int64{"e2-micro": 250, "e2-small": 500, "e2-medium": 1000}

// sharedCoreCost is per hour; the shapes are not priced per vCPU and GiB like the rest of their family.
var sharedCoreCost = map[string]float64{"e2-micro": 0.00838, "e2-small": 0.01675, "e2-medium": 0.03351}

// familyRates are on-demand $/vCPU-hour and $/GiB-hour (us-central1). Only their ratios matter: they rank the shapes
// that fit, they never bill anything.
var familyRates = map[string][2]float64{
	"e2":  {0.021811, 0.002923},
	"n2":  {0.031611, 0.004237},
	"n2d": {0.027502, 0.003686},
	"t2d": {0.027502, 0.004400},
	"t2a": {0.024200, 0.003300},
}

// FamilyArch is the architecture of every family the driver runs on. They all attach pd-balanced, the disk type the
// driver creates, so a stopped VM can move between them.
var FamilyArch = map[string]string{"e2": "amd64", "n2": "amd64", "n2d": "amd64", "t2d": "amd64", "t2a": "arm64"}

func family(machineType string) string {
	f, _, _ := strings.Cut(machineType, "-")
	return f
}

func sustainedMillis(t gceapi.MachineType) int64 {
	if m, ok := sharedCoreMillis[t.Name]; ok {
		return m
	}
	return t.GuestCpus * 1000
}

func hourlyCost(t gceapi.MachineType) float64 {
	if c, ok := sharedCoreCost[t.Name]; ok {
		return c
	}
	r := familyRates[family(t.Name)]
	return float64(t.GuestCpus)*r[0] + float64(t.MemoryMb)/1024*r[1]
}

// PickShape returns the cheapest shape in family with at least cpuMillis sustained CPU and memory bytes, from the
// zone's machine types plus E2 custom shapes.
func PickShape(fam string, cpuMillis, memory int64, types []gceapi.MachineType) (string, error) {
	if _, ok := familyRates[fam]; !ok {
		return "", fmt.Errorf("machine family %q is not supported (supported: e2, n2, n2d, t2d, t2a)", fam)
	}
	var best *gceapi.MachineType
	consider := func(t gceapi.MachineType) {
		if family(t.Name) != fam || sustainedMillis(t) < cpuMillis || t.MemoryMb*mib < memory {
			return
		}
		if best == nil || hourlyCost(t) < hourlyCost(*best) ||
			hourlyCost(t) == hourlyCost(*best) && t.MemoryMb < best.MemoryMb {
			c := t
			best = &c
		}
	}
	for _, t := range types {
		consider(t)
	}
	if fam == "e2" {
		if t, ok := e2Custom(cpuMillis, memory); ok {
			consider(t)
		}
	}
	if best == nil {
		return "", fmt.Errorf("no %s shape has %d millicpu and %d MiB", fam, cpuMillis, (memory+mib-1)/mib)
	}
	return best.Name, nil
}

// e2Custom follows E2's custom-shape rules: an even vCPU count from 2 to 32, 0.5 to 8 GiB per vCPU, in 256 MiB steps.
func e2Custom(cpuMillis, memory int64) (gceapi.MachineType, bool) {
	cpus := max(2, (cpuMillis+999)/1000)
	cpus += cpus % 2
	memMb := (memory + mib - 1) / mib
	memMb = (memMb + 255) / 256 * 256
	memMb = max(memMb, cpus*512)
	for memMb > cpus*8192 {
		cpus += 2
	}
	if cpus > 32 {
		return gceapi.MachineType{}, false
	}
	name := "e2-custom-" + strconv.FormatInt(cpus, 10) + "-" + strconv.FormatInt(memMb, 10)
	return gceapi.MachineType{Name: name, GuestCpus: cpus, MemoryMb: memMb}, true
}
