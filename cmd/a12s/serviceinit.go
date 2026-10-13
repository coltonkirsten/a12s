package main

import (
	"os"

	"github.com/coltonkirsten/a12s/internal/serviceinit"
)

// hidden are in-container helpers, not listed in usage.
func hidden(name string, args []string) (error, bool) {
	switch name {
	case "service-init":
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		return serviceinit.Init(args, os.Stderr), true
	case "service-write":
		if len(args) != 4 {
			return usageError("service-write <path> <uid:gid> <mode> <dirMode> < data"), true
		}
		return serviceinit.WriteFile(args[0], args[1], args[2], args[3], os.Stdin), true
	case "service-forward":
		if len(args) < 2 {
			return usageError("service-forward <proxy> <target>..."), true
		}
		return serviceinit.ServeForwards(args[0], args[1:], os.Stderr), true
	}
	return nil, false
}
