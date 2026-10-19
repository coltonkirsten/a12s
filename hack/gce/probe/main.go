// Command probe is the GCE exit gate's view into an agent computer's VM, run as root on the Controller VM (it reads
// the Controller's CA from its state volume):
//
//	probe <fleet> exec <computer> <command>...   run a command in the container as the agent; prints output, exits with its code
//	probe <fleet> stats <computer>               print the container's memory and CPU, and the engine's image and volume bytes, as JSON
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/cli"
	"github.com/coltonkirsten/a12s/internal/controller"
	"github.com/coltonkirsten/a12s/internal/driver/engine"
	"github.com/coltonkirsten/a12s/internal/driver/gce"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(2)
	}
}

func run(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("usage: probe <fleet> exec <computer> <command>... | probe <fleet> stats <computer>")
	}
	fleet, verb, computer := args[0], args[1], args[2]
	state := "/var/lib/docker/volumes/a12s-" + fleet + "-controller/_data"
	if _, err := os.Stat(filepath.Join(state, "ca.key")); err != nil {
		return fmt.Errorf("the Controller's CA: %w", err)
	}
	ca, err := controller.LoadCA(state, fleet)
	if err != nil {
		return err
	}
	leaf, err := ca.ControllerClientCert()
	if err != nil {
		return err
	}
	cert, err := leaf.TLS()
	if err != nil {
		return err
	}
	// Under sudo, keep HOME pointing at the operator's home, where driver.yaml is.
	drv, err := cli.ReadDriverConfig(fleet)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	g, ec, err := gce.Probe(ctx, fleet, drv.GCE, cert, computer)
	if err != nil {
		return err
	}
	defer g.Close()
	switch verb {
	case "exec":
		code, out, err := g.Exec(ctx, engine.AgentUser, args[3:], nil)
		if err != nil {
			return err
		}
		fmt.Print(out)
		os.Exit(code)
	case "stats":
		return stats(ctx, ec, "a12s-"+fleet+"-"+computer)
	}
	return fmt.Errorf("unknown verb %q", verb)
}

func stats(ctx context.Context, ec *client.Client, name string) error {
	res, err := ec.ContainerStats(ctx, name, client.ContainerStatsOptions{Stream: false})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var s struct {
		MemoryStats struct {
			Usage uint64            `json:"usage"`
			Stats map[string]uint64 `json:"stats"`
		} `json:"memory_stats"`
		CPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
		} `json:"cpu_stats"`
	}
	if err := json.NewDecoder(res.Body).Decode(&s); err != nil {
		return err
	}
	df, err := ec.DiskUsage(ctx, client.DiskUsageOptions{})
	if err != nil {
		return err
	}
	out := map[string]any{
		"memoryBytes":   s.MemoryStats.Usage - s.MemoryStats.Stats["inactive_file"],
		"cpuTotalNanos": s.CPUStats.CPUUsage.TotalUsage,
		"imagesBytes":   df.Images.TotalSize,
		"volumesBytes":  df.Volumes.TotalSize,
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}
