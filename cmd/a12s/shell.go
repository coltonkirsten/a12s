package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/coltonkirsten/a12s/internal/api"
)

// runShell asks the Controller to audit the shell (it journals a lifecycle
// event and names the container), then replaces this process with a local
// `docker exec` (PLAN "Spec deviations: Shell").
func runShell(g *globals, args []string) error {
	fs := g.flags("shell")
	root := fs.Bool("root", false, "run as root instead of the agent user")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return usageError("shell needs a Computer name")
	}
	name, command := pos[0], pos[1:]
	docker, err := exec.LookPath("docker")
	if err != nil {
		return fmt.Errorf("a12s shell runs the docker CLI: %w", err)
	}
	c, _, err := g.connect()
	if err != nil {
		return err
	}
	ctx, cancel := interruptible()
	defer cancel()
	var resp api.ShellResponse
	if err := c.Do(ctx, "POST", computerPath(name, "/shell"), api.ShellRequest{Root: *root, Command: command}, &resp); err != nil {
		return err
	}
	cancel()
	user := resp.User
	if user == "" {
		user = "1000:1000"
		if *root {
			user = "0:0"
		}
	}
	argv := []string{"docker", "exec", "-i"}
	if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		argv = append(argv, "-t")
	}
	argv = append(argv, "-u", user, "-w", "/home/agent", resp.Container)
	if len(command) == 0 {
		command = []string{"sh", "-c", "if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi"}
	}
	argv = append(argv, command...)
	return syscall.Exec(docker, argv, os.Environ())
}
