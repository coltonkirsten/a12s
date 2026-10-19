package gce

import (
	_ "embed"
	"net/netip"
	"strings"

	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
)

//go:embed boot.sh
var bootScript string

// Metadata keys a Computer VM boots with. None holds a secret: anyone who can read the instance can read them, and
// so can every process on the VM.
const (
	mdController = "a12s-controller"
	mdCA         = "a12s-ca"
	mdIdentity   = "a12s-identity"
)

// BootMetadata is the same for every Computer of a fleet: cloud-init that runs boot.sh on every boot, the
// Controller's address, the Fleet CA's certificate (public), and whether Computers have their own identity.
func BootMetadata(controller netip.Addr, caPEM []byte, identity string) []gceapi.MetadataItem {
	var b strings.Builder
	b.WriteString("#cloud-config\nwrite_files:\n- path: /var/lib/a12s/boot.sh\n  owner: root\n  permissions: \"0700\"\n  content: |\n")
	for _, line := range strings.SplitAfter(bootScript, "\n") {
		if line != "" {
			b.WriteString("    " + line)
		}
	}
	// The stateful partition is mounted noexec, so the script runs through bash.
	b.WriteString("runcmd:\n- bash /var/lib/a12s/boot.sh\n")
	return []gceapi.MetadataItem{
		{Key: "user-data", Value: b.String()},
		{Key: mdController, Value: controller.String()},
		{Key: mdCA, Value: string(caPEM)},
		{Key: mdIdentity, Value: identity},
		{Key: "enable-guest-attributes", Value: "TRUE"},
		// Nobody logs in to a Computer VM; project-wide SSH keys would let them.
		{Key: "block-project-ssh-keys", Value: "TRUE"},
	}
}
