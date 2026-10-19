package gce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
)

const (
	IdentityNone        = "none"
	IdentityPerComputer = "per-computer"

	cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
	metadataServer     = "169.254.169.254"
)

// ensureIdentity gives a Computer its own service account: no keys, ever, and no roles. The app grants it what it
// needs, and Cloud Audit Logs name the agent for every Google API call it makes.
func (d *Driver) ensureIdentity(ctx context.Context, computer string) (string, error) {
	id := ServiceAccountID(d.fleet, computer)
	sa, err := d.api.CreateServiceAccount(ctx, id, "a12s "+d.fleet+"/"+computer)
	if gceapi.IsConflict(err) {
		sa, err = d.api.GetServiceAccount(ctx, ServiceAccountEmail(d.fleet, computer, d.api.Project))
	}
	if err != nil {
		return "", fmt.Errorf("service account %s: %w", id, err)
	}
	return sa.Email, nil
}

// removeIdentity deletes the account. A recreated account with the same email is a new principal: grants made to
// the old one don't carry over, so a re-hired agent starts with no access.
func (d *Driver) removeIdentity(ctx context.Context, computer string) error {
	err := d.api.DeleteServiceAccount(ctx, ServiceAccountEmail(d.fleet, computer, d.api.Project))
	if err != nil && !gceapi.IsNotFound(err) {
		return fmt.Errorf("delete service account: %w", err)
	}
	return nil
}

// identityRetry paces retries while a new service account isn't usable yet: IAM is eventually consistent.
var identityRetry = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}

func (d *Driver) insert(ctx context.Context, in *gceapi.Instance) error {
	for i := 0; ; i++ {
		err := d.api.InsertInstance(ctx, in)
		if err == nil || len(in.ServiceAccounts) == 0 || i == len(identityRetry) || !accountNotYetUsable(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(identityRetry[i]):
		}
	}
}

func accountNotYetUsable(err error) bool {
	var e *gceapi.APIError
	return errors.As(err, &e) && e.Status == 400 && strings.Contains(e.Message, "serviceAccount")
}

// IdentityEnv lets a Computer's harness and tools reach the metadata server, which hands out its account's tokens,
// directly: the egress proxy refuses metadata addresses, and the in-VM firewall admits only its HTTP port.
func IdentityEnv() []string {
	b, _ := json.Marshal(map[string]string{
		"GCE_METADATA_HOST": metadataServer,
		"GCE_METADATA_IP":   metadataServer,
		"NO_PROXY":          metadataServer,
		"no_proxy":          metadataServer,
	})
	return []string{"A12S_HARNESS_ENV=" + string(b)}
}
