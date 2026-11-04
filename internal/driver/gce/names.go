package gce

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// GCE labels: keys and values of lowercase letters, digits, '_' and '-', up to 63 characters. a12s names already fit.
const (
	LabelFleet      = "a12s-fleet"
	LabelComputer   = "a12s-computer"
	LabelRole       = "a12s-role"
	LabelGeneration = "a12s-generation"
	LabelSpec       = "a12s-spec" // digest of the staged spec
	LabelDriveKind  = "a12s-drive"
	// LabelExpires is the Unix time after which a retired home's snapshot is deleted.
	LabelExpires = "a12s-expires"
	// LabelFailure records why a Computer stopped on its own: "oom", "exit-<code>", "host-error",
	// "guest-terminate" or "no-capacity". Its VM is stopped then, so its engine can't be asked.
	LabelFailure = "a12s-failure"

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

// SnapshotName is unique per disk, so retiring the same disk again finds its snapshot instead of making another.
func SnapshotName(disk string, diskID uint64) string {
	return fitName(disk + "-" + strconv.FormatUint(diskID, 10))
}

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
