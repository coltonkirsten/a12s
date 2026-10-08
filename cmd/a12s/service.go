package main

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/cli"
)

func runService(g *globals, args []string) error {
	if len(args) == 0 {
		return usageError("service needs a subcommand: ls, status, logs, login")
	}
	switch args[0] {
	case "ls":
		return serviceLs(g, args[1:])
	case "status":
		return serviceStatus(g, args[1:])
	case "logs":
		return serviceLogs(g, args[1:])
	case "login":
		return serviceLogin(g, args[1:])
	}
	return usageError("unknown service subcommand " + args[0])
}

func servicePath(name, suffix string) string {
	return "/v1/services/" + url.PathEscape(name) + suffix
}

func serviceLs(g *globals, args []string) error {
	fs := g.flags("service ls")
	asJSON := fs.Bool("json", false, "print as JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError("service ls takes no arguments")
	}
	c, _, err := g.connect()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	var resp api.ServicesResponse
	if err := c.Do(ctx, "GET", "/v1/services", nil, &resp); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(resp)
	}
	if len(resp.Services) == 0 {
		fmt.Println("no Services")
		return nil
	}
	cli.WriteServices(os.Stdout, resp.Services)
	return nil
}

func serviceStatus(g *globals, args []string) error {
	fs := g.flags("service status")
	asJSON := fs.Bool("json", false, "print as JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("service status needs one Service name")
	}
	c, _, err := g.connect()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	var s api.ServiceStatus
	if err := c.Do(ctx, "GET", servicePath(pos[0], ""), nil, &s); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(s)
	}
	cli.WriteServiceDetail(os.Stdout, s)
	return nil
}

func serviceLogs(g *globals, args []string) error {
	// -f follows here (§6.3: the global -f becomes --fleet/-F).
	fs := flag.NewFlagSet("service logs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&g.fleetPath, "fleet", g.fleetPath, "Fleet file")
	fs.StringVar(&g.fleetPath, "F", g.fleetPath, "Fleet file")
	fs.StringVar(&g.bindingsPath, "bindings", g.bindingsPath, "bindings file")
	var follow bool
	fs.BoolVar(&follow, "f", false, "follow")
	fs.BoolVar(&follow, "follow", false, "follow")
	tail := fs.Int("tail", 0, "only the last N lines of each part")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("service logs needs one Service name")
	}
	c, _, err := g.connect()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	q := url.Values{}
	if follow {
		q.Set("follow", "true")
	}
	if *tail > 0 {
		q.Set("tail", strconv.Itoa(*tail))
	}
	req, err := c.NewRequest(ctx, "GET", servicePath(pos[0], "/logs?"+q.Encode()), nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return cli.ResponseError(resp)
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// splitTargets separates names into Computers and Services by the Fleet
// file; an unknown name is an error.
func splitTargets(names []string, isService func(string) bool, isComputer func(string) bool) (computers, services []string, err error) {
	var unknown []string
	for _, n := range names {
		switch {
		case isService(n):
			services = append(services, n)
		case isComputer(n):
			computers = append(computers, n)
		default:
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		return nil, nil, fmt.Errorf("no Computer or Service named %s in the Fleet file", strings.Join(unknown, ", "))
	}
	return computers, services, nil
}
