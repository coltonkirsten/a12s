package controller

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/coltonkirsten/a12s/internal/api"
)

const (
	secretsDir     = "/run/a12s/secrets"
	tlsDir         = "/run/a12s/tls"
	generationFile = secretsDir + "/.generation"
)

// Directory modes, root-owned: the agent may traverse secrets/ to open its own credential
// files but cannot create, rename or replace entries in it; tls/ is closed to it.
const (
	secretsDirMode = "0711"
	tlsDirMode     = "0700"
)

// writeScript: root-only temp file then rename, so nothing is written through an existing path
// (a symlink) and readers never see a partial file. The rename and the mtime touch hold the root-only
// .lock flock that computerd's credential reclaim takes for its compare-and-swap; the touch makes a
// prior harness refresh read as older, and if it fails the file is removed so a secret with a stale
// mtime is never left in place. An image without flock(1) renames unlocked.
const writeScript = `umask 077; d="${1%/*}"; t="$d/.a12s-write.$$"; l="$d/.lock"
mkdir -p "$d" && chown 0:0 "$d" && chmod "$4" "$d" &&
cat > "$t" && chown "$2" "$t" && chmod "$3" "$t" &&
if command -v flock > /dev/null 2>&1; then
  : >> "$l" && chown 0:0 "$l" && chmod 0600 "$l" && flock -w 10 "$l" sh -c 'mv -f "$1" "$2" && { touch "$2" || { rm -f "$2"; false; }; }' sh "$t" "$1"
else mv -f "$t" "$1" && { touch "$1" || { rm -f "$1"; false; }; }; fi || { rm -f "$t"; exit 1; }`

// inject writes .generation last: computerd waits for the TLS files and reports it as
// credentialGeneration. A fresh certificate is registered live before it is written.
func (c *Controller) inject(ctx context.Context, name string, withCert bool) error {
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
	var gen, generation int64
	var send []string
	if rec != nil {
		gen, generation, send = rec.CredentialGeneration, rec.Generation, slices.Clone(rec.Active.Send)
	}
	c.mu.Unlock()
	switch {
	case !ok || rec == nil:
		return fmt.Errorf("inject %s: not in the Fleet", name)
	case len(missing) > 0:
		return &credentialsMissing{credentialWait(missing)}
	case err != nil:
		return err
	}

	for _, cred := range comp.Credentials {
		if err := c.writeFile(ctx, name, secretsDir+"/"+cred, "1000:1000", "0600", secretsDirMode, material[cred]); err != nil {
			return err
		}
	}
	if !withCert {
		return c.writeFile(ctx, name, generationFile, "0:0", "0644", secretsDirMode, []byte(strconv.FormatInt(gen, 10)+"\n"))
	}
	p := Principal{Kind: KindComputer, Name: name, Generation: generation}
	leaf, err := c.ca.ComputerCert(name, generation)
	if err != nil {
		return fmt.Errorf("inject %s: %w", name, err)
	}
	if err := c.registerCert(ctx, p, leaf.Serial, send); err != nil {
		return fmt.Errorf("inject %s: register certificate: %w", name, err)
	}
	for _, f := range []struct {
		name string
		data []byte
	}{{"ca.crt", c.ca.CertPEM()}, {"cert.pem", leaf.CertPEM}, {"key.pem", leaf.KeyPEM}} {
		if err := c.writeFile(ctx, name, tlsDir+"/"+f.name, "0:0", "0600", tlsDirMode, f.data); err != nil {
			return err
		}
	}
	return c.writeFile(ctx, name, generationFile, "0:0", "0644", secretsDirMode, []byte(strconv.FormatInt(gen, 10)+"\n"))
}

func (c *Controller) writeFile(ctx context.Context, computer, path, owner, mode, dirMode string, data []byte) error {
	cmd := []string{"sh", "-c", writeScript, "sh", path, owner, mode, dirMode}
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

// credentialsMissing is not a failure: the agent computer or Service waits for the values.
type credentialsMissing struct{ waiting *api.Waiting }

func (e *credentialsMissing) Error() string { return e.waiting.String() }

// credentialWait says how to set each missing credential.
func credentialWait(missing []string) *api.Waiting {
	var fixes []string
	for _, n := range missing {
		fixes = append(fixes, "a12s creds set "+n)
	}
	return &api.Waiting{Credentials: slices.Clone(missing), Fix: strings.Join(fixes, "; ")}
}

func sameWaiting(a, b *api.Waiting) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Fix == b.Fix && slices.Equal(a.Credentials, b.Credentials)
}
