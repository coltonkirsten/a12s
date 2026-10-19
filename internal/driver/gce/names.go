package gce

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// GCE labels: keys and values of lowercase letters, digits, '_' and '-', up to 63 characters. a12s names already fit.
const (
	LabelFleet      = "a12s-fleet"
	LabelComputer   = "a12s-computer"
	LabelRole       = "a12s-role"
	LabelGeneration = "a12s-generation"
	LabelSpec       = "a12s-spec" // digest of the staged spec
	LabelExit       = "a12s-exit" // exit code of a container that died while the driver watched
	LabelOOM        = "a12s-oom"
	LabelDriveKind  = "a12s-drive"

	RoleComputer = "computer"
)

const maxName = 63

// fitName keeps names readable and unique: a name over 63 characters becomes a prefix plus a hash of the whole.
func fitName(name string) string {
	if len(name) <= maxName {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	prefix := strings.TrimRight(name[:maxName-9], "-")
	return prefix + "-" + hex.EncodeToString(sum[:4])
}

func InstanceName(fleet, computer string) string { return fitName("a12s-" + fleet + "-" + computer) }

func DiskName(fleet, driveID string) string { return fitName("a12s-" + fleet + "-" + driveID) }

// ServiceAccountID is 6 to 30 characters, starts with a letter and never ends with '-'. The hash keeps two
// Computers whose names share a long prefix apart.
func ServiceAccountID(fleet, computer string) string {
	base := fleet + "-" + computer
	if base[0] < 'a' || base[0] > 'z' {
		base = "a" + base
	}
	if len(base) > 25 {
		base = base[:25]
	}
	base = strings.TrimRight(base, "-")
	sum := sha256.Sum256([]byte(fleet + "/" + computer))
	return base + "-" + hex.EncodeToString(sum[:2])
}

func ServiceAccountEmail(fleet, computer, project string) string {
	return ServiceAccountID(fleet, computer) + "@" + project + ".iam.gserviceaccount.com"
}
