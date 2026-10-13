package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

const defaultConsolePackage = "github:coltonkirsten/a12s-console"

// runUI starts the web console with npx. Every argument after `ui` is the console's own; the Fleet comes from
// -F before `ui`, or the current directory, like every command.
func runUI(g *globals, args []string) error {
	_, f, err := g.connect()
	if err != nil {
		return err
	}
	npx, err := exec.LookPath("npx")
	if err != nil {
		return errors.New("npx is not on the PATH  fix: install Node.js 20+ (it includes npx)")
	}
	pkg := defaultConsolePackage
	if p := os.Getenv("A12S_CONSOLE"); p != "" {
		pkg = p
	}
	cmd := exec.Command(npx, append([]string{"--yes", pkg, "--fleet", f.Metadata.Name}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The console gets Ctrl-C from the terminal too; a12s only passes signals on and waits for it to exit.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the console: %w", err)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case s := <-sigs:
				_ = cmd.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()
	err = cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code > 0 {
			return exitCode(code)
		}
		return exitCode(130)
	}
	return err
}
