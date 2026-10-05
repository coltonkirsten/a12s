package main

// runControllerCmd is `a12s controller`: the Controller itself, run as the
// entrypoint of its container (cmd/a12s/controller.go).
func runControllerCmd(_ *globals, args []string) error { return runController(args) }
