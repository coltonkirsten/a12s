package controller

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Paths inside a Computer (PLAN "Secrets and TLS").
const (
	secretsDir     = "/run/a12s/secrets"
	tlsDir         = "/run/a12s/tls"
	generationFile = secretsDir + "/.generation"
)

// writeScript writes stdin to $1 owned by $2 with mode $3, never readable by
// anyone else in between. The directory is created when computerd has not
// made it yet; computerd fixes its owner and mode when it does.
const writeScript = `umask 077; mkdir -p "${1%/*}" && cat > "$1" && chown "$2" "$1" && chmod "$3" "$1"`

// inject writes the Computer's credentials and a fresh certificate into its
// tmpfs through Exec with stdin, then .generation last: computerd waits for
// the TLS files and reports .generation as credentialGeneration.
func (c *Controller) inject(ctx context.Context, name string) error {
	c.mu.Lock()
	comp, ok := c.state.Fleet.Computers[name]
	rec := c.state.Computers[name]
	material := map[string][]byte{}
	for _, cred := range comp.Credentials {
		material[cred] = c.creds[cred]
	}
	missing := missingCredentials(comp.Credentials, material)
	var err error
	if ok && rec != nil && len(missing) == 0 {
		rec.CredentialGeneration++
		err = c.saveLocked()
	}
	var gen int64
	if rec != nil {
		gen = rec.CredentialGeneration
	}
	c.mu.Unlock()
	switch {
	case !ok || rec == nil:
		return fmt.Errorf("inject %s: not in the Fleet", name)
	case len(missing) > 0:
		return &credentialsMissing{names: missing}
	case err != nil:
		return err
	}

	for _, cred := range comp.Credentials {
		if err := c.writeFile(ctx, name, secretsDir+"/"+cred, "1000:1000", "0600", material[cred]); err != nil {
			return err
		}
	}
	leaf, err := c.ca.ComputerCert(name)
	if err != nil {
		return fmt.Errorf("inject %s: %w", name, err)
	}
	for _, f := range []struct {
		name string
		data []byte
	}{{"ca.crt", c.ca.CertPEM()}, {"cert.pem", leaf.CertPEM}, {"key.pem", leaf.KeyPEM}} {
		if err := c.writeFile(ctx, name, tlsDir+"/"+f.name, "0:0", "0600", f.data); err != nil {
			return err
		}
	}
	return c.writeFile(ctx, name, generationFile, "0:0", "0644", []byte(strconv.FormatInt(gen, 10)+"\n"))
}

func (c *Controller) writeFile(ctx context.Context, computer, path, owner, mode string, data []byte) error {
	cmd := []string{"sh", "-c", writeScript, "sh", path, owner, mode}
	code, out, err := c.drv.Exec(ctx, computer, cmd, bytes.NewReader(data), true)
	if err != nil {
		return fmt.Errorf("inject %s: write %s: %w", computer, path, err)
	}
	if code != 0 {
		return fmt.Errorf("inject %s: write %s: exit %d: %s", computer, path, code, strings.TrimSpace(out))
	}
	return nil
}

func missingCredentials(names []string, have map[string][]byte) []string {
	var missing []string
	for _, n := range names {
		if len(have[n]) == 0 {
			missing = append(missing, n)
		}
	}
	return missing
}

// credentialsMissing is returned when a Computer must start but the
// Controller holds no material for some of its credentials (for example
// after a Controller restart).
type credentialsMissing struct{ names []string }

func (e *credentialsMissing) Error() string {
	return "credentials missing (" + strings.Join(e.names, ", ") + "), run a12s up"
}
