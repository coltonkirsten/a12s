package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/cli"
	"github.com/coltonkirsten/a12s/internal/fleet"
	"github.com/coltonkirsten/a12s/internal/version"
)

type check struct {
	Level string `json:"level"` // ok, warn, fail
	What  string `json:"what"`
	Fix   string `json:"fix,omitempty"`
}

func runDoctor(g *globals, args []string) error {
	pos, err := parse(g.flags("doctor"), args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError("doctor takes no arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	checks := doctor(ctx, g)
	failed := 0
	if g.json {
		if err := printJSON(map[string]any{"checks": checks}); err != nil {
			return err
		}
	}
	for _, c := range checks {
		if c.Level == "fail" {
			failed++
		}
		if g.json {
			continue
		}
		line := fmt.Sprintf("%-4s  %s", c.Level, c.What)
		if c.Fix != "" {
			line += "  fix: " + c.Fix
		}
		fmt.Println(line)
	}
	if failed > 0 {
		return exitCode(1)
	}
	return nil
}

func doctor(ctx context.Context, g *globals) []check {
	var out []check
	ok := func(f string, a ...any) { out = append(out, check{"ok", fmt.Sprintf(f, a...), ""}) }
	warn := func(fix, f string, a ...any) { out = append(out, check{"warn", fmt.Sprintf(f, a...), fix}) }
	bad := func(fix, f string, a ...any) { out = append(out, check{"fail", fmt.Sprintf(f, a...), fix}) }

	f, err := fleet.Load(g.fleetPath)
	if err != nil {
		bad("fix the Fleet file (or point at it with --fleet)", "%v", err)
		return out
	}
	ok("fleet %s: %d agent computer(s), %d Service(s)", f.Metadata.Name, len(f.Computers), len(f.Services))

	dc, err := client.New(client.FromEnv)
	if err == nil {
		defer dc.Close()
		var info client.SystemInfoResult
		if info, err = dc.Info(ctx, client.InfoOptions{}); err == nil {
			ok("docker %s (%s, %d CPUs, %d MiB)", info.Info.ServerVersion, info.Info.Architecture, info.Info.NCPU, info.Info.MemTotal>>20)
		}
	}
	if err != nil {
		bad("start Docker Desktop (or set DOCKER_HOST)", "docker is not reachable: %v", err)
	} else {
		have := func(ref string) bool {
			_, err := dc.ImageInspect(ctx, ref)
			return err == nil
		}
		seen := map[string]bool{}
		for _, name := range f.ComputerNames() {
			c := f.Computers[name]
			if img := c.Image; !seen["i"+img] {
				seen["i"+img] = true
				ref := img
				if img == fleet.TierSlim {
					ref = version.Image(img)
				}
				if have(ref) {
					ok("image %s", ref)
				} else {
					bad(fetchFix("make image-"+img, ref), "image %s is not on this machine", ref)
				}
			}
			if k := c.Harness.BundleKey(); !seen["b"+k] {
				seen["b"+k] = true
				ref := version.Bundle(c.Harness.Kind, c.Harness.Version)
				if have(ref) {
					ok("bundle %s", k)
				} else {
					bad(fetchFix(fmt.Sprintf("make bundle-%s VERSION=%s", c.Harness.Kind, c.Harness.Version), ref), "bundle %s is not on this machine", k)
				}
			}
		}
		for _, name := range f.ServiceNames() {
			s := f.Services[name]
			if have(s.Image) {
				ok("service image %s", s.Image)
			} else {
				bad("docker pull "+s.Image, "service image %s is not on this machine", s.Image)
			}
		}
		if have(cli.DefaultControllerImage) {
			ok("controller image %s", cli.DefaultControllerImage)
		} else {
			bad(fetchFix("make image-controller", cli.DefaultControllerImage), "controller image %s is not on this machine", cli.DefaultControllerImage)
		}
	}

	for _, name := range f.ServiceNames() {
		s := f.Services[name]
		if fetchesAtStart(s.Config) {
			warn("bake the server into a derived image and point config.command at it (docs/services.md)",
				"services.%s.config runs npx at start, which fetches unpinned code at every start", name)
		}
	}

	c, err := cli.Dial(f.Metadata.Name)
	if err != nil {
		warn("a12s up", "no Controller for fleet %s", f.Metadata.Name)
		return out
	}
	var st api.StatusResponse
	if err := c.Do(ctx, "GET", "/v1/status", nil, &st); err != nil {
		bad("a12s up (it restarts the Controller)", "the Controller does not answer: %v", err)
		return out
	}
	ok("controller answers (fleet %s)", st.Fleet)
	if !st.CredentialsLoaded {
		warn("a12s creds set <name>", "the Controller holds no credentials (a memory-only Controller restarted)")
	}
	if st.EgressEnforcement == fleet.EnforcementUnenforced {
		warn("set egress.enforcement: enforced on a substrate that can enforce it", "egress is not enforced for this Fleet")
	} else {
		ok("egress proxy enforcing")
	}
	for _, cs := range st.AgentComputers {
		if cs.Waiting != nil {
			warn(cs.Waiting.Fix, "%s waits: %s", cs.Name, cs.Waiting)
			continue
		}
		switch cs.Phase {
		case api.PhaseError:
			reason := ""
			if cs.Error != nil {
				reason = cs.Error.Reason
			}
			bad("a12s wake "+cs.Name+" (after fixing the cause)", "%s is in Error: %s", cs.Name, reason)
		case api.PhaseReady, api.PhaseBusy, api.PhaseAsleep:
			ok("%s %s", cs.Name, cs.Phase)
		default:
			warn("a12s status "+cs.Name, "%s is %s", cs.Name, cs.Phase)
		}
	}
	for _, ss := range st.Services {
		if ss.Waiting != nil {
			warn(ss.Waiting.Fix, "service %s waits: %s", ss.Name, ss.Waiting)
			continue
		}
		switch ss.Phase {
		case api.PhaseReady, api.PhaseAsleep:
			ok("service %s %s", ss.Name, ss.Phase)
		case api.PhaseError:
			reason := ""
			if ss.Error != nil {
				reason = ss.Error.Reason
			}
			m, fix, _ := strings.Cut(reason, "  fix: ")
			if fix == "" {
				fix = "a12s service logs " + ss.Name
			}
			bad(fix, "service %s is in Error: %s", ss.Name, m)
		default:
			h := ss.Health
			if h == "" {
				h = "no health result yet"
			}
			warn("a12s service logs "+ss.Name, "service %s is %s (%s)", ss.Name, ss.Phase, h)
		}
	}
	_ = os.Stdout
	return out
}

// fetchesAtStart flags npx, which fetches at start instead of running what the image pinned.
func fetchesAtStart(cfg map[string]any) bool {
	var words []string
	collect := func(v any) {
		switch x := v.(type) {
		case string:
			words = append(words, x)
		case []any:
			for _, e := range x {
				if s, ok := e.(string); ok {
					words = append(words, s)
				}
			}
		}
	}
	collect(cfg["command"])
	collect(cfg["args"])
	for _, w := range words {
		base := w[strings.LastIndex(w, "/")+1:]
		if base == "npx" || strings.HasPrefix(w, "npx ") {
			return true
		}
	}
	return false
}

// fetchFix is the build target in a source build and a pull in a release, whose images come from the registry.
func fetchFix(makeTarget, ref string) string {
	if version.Release() {
		return "docker pull " + ref
	}
	return makeTarget
}
