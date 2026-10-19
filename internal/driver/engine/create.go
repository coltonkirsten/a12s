package engine

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
)

// Placement is what the substrate decides about a Computer's container.
type Placement struct {
	Name    string // container name
	Network string // the network it joins, and its only one
	// Drive mounts a drive at target: the home drive at /home, a keep drive inside an attached folder.
	Drive        func(driveID, target string) mount.Mount
	BundleVolume string
	// ProxyURL and ControllerURL apply when the spec leaves them empty.
	ProxyURL, ControllerURL string
	Ports                   network.PortMap
	// Limits applies the spec's CPU and memory to the container; a substrate that sizes the machine to the spec
	// leaves it off.
	Limits bool
	// Env is what the substrate adds to the spec's environment.
	Env []string
}

// VolumeMount holds only what was written to the volume, never image content.
func VolumeMount(source, target string, readOnly bool) mount.Mount {
	return mount.Mount{
		Type: mount.TypeVolume, Source: source, Target: target, ReadOnly: readOnly,
		VolumeOptions: &mount.VolumeOptions{NoCopy: true},
	}
}

func (e *Engine) CreateOptions(spec driver.ComputerSpec, p Placement) client.ContainerCreateOptions {
	if spec.ProxyURL == "" {
		spec.ProxyURL = p.ProxyURL
	}
	if spec.ControllerURL == "" {
		spec.ControllerURL = p.ControllerURL
	}
	mounts := []mount.Mount{
		p.Drive(spec.HomeDrive, HomeMount),
		VolumeMount(p.BundleVolume, driver.BundleRoot, true),
	}
	for _, a := range spec.Attach {
		// CreateMountpoint stays off: a missing host folder must fail, not appear empty.
		mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: a.Host, Target: a.Target, ReadOnly: a.ReadOnly})
		for _, k := range a.Keep {
			mounts = append(mounts, p.Drive(k.Drive, k.Target))
		}
	}
	security := []string{"no-new-privileges:true"}
	if spec.Seccomp != "" {
		security = append(security, "seccomp="+spec.Seccomp)
	}
	stop := StopTimeout
	useInit := true
	var res container.Resources
	if p.Limits {
		res = container.Resources{Memory: spec.Memory, NanoCPUs: spec.CPUMillis * 1_000_000}
	}
	return client.ContainerCreateOptions{
		Name: p.Name,
		Config: &container.Config{
			Image:       spec.Image,
			Hostname:    spec.Name,
			Env:         append(spec.Env(e.fleet), p.Env...),
			StopTimeout: &stop,
			Healthcheck: NoHealthcheck,
			// A custom image's ENTRYPOINT or USER must not replace computerd, which starts as root and drops the
			// harness to uid 1000 itself.
			Entrypoint: []string{"node", driver.ComputerdMain},
			User:       "0:0",
			Labels: map[string]string{
				LabelFleet:      e.fleet,
				LabelComputer:   spec.Name,
				LabelGeneration: strconv.FormatInt(spec.Generation, 10),
				LabelRole:       RoleComputer,
			},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    container.NetworkMode(p.Network),
			RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyDisabled},
			Init:           &useInit, // --init: reap orphans, forward signals to computerd
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			CapAdd:         capAdd,
			SecurityOpt:    security,
			Tmpfs: map[string]string{
				runA12S: "rw,nosuid,nodev,noexec,mode=0700,size=16m",
				"/tmp":  "rw,nosuid,nodev,mode=1777",
			},
			Mounts:       mounts,
			PortBindings: p.Ports,
			Resources:    res,
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{p.Network: {}},
		},
	}
}

var MountPointRE = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

func CheckSpec(s driver.ComputerSpec) error {
	switch {
	case s.Name == "":
		return errors.New("name is required")
	case s.Generation < 1:
		return errors.New("generation must be positive")
	case s.Image == "":
		return errors.New("image is required")
	case s.HomeDrive == "":
		return errors.New("home drive is required")
	case s.Bundle.Kind == "" || s.Bundle.Version == "":
		return errors.New("bundle kind and version are required")
	case driver.ShortImageID(s.Bundle.Image) == "":
		return fmt.Errorf("bundle image %q must be the image ID EnsureBundle returned", s.Bundle.Image)
	}
	for _, a := range s.Attach {
		if !path.IsAbs(a.Host) || path.Clean(a.Host) != a.Host || a.Host == "/" {
			return fmt.Errorf("attach host %q must be a clean absolute path other than /", a.Host)
		}
		if !MountPointRE.MatchString(a.Target) || path.Clean(a.Target) != a.Target || path.Dir(a.Target) != agentHome {
			return fmt.Errorf("attach target %q must be a folder directly in %s", a.Target, agentHome)
		}
		for _, k := range a.Keep {
			if k.Drive == "" || !MountPointRE.MatchString(k.Target) || path.Clean(k.Target) != k.Target || !strings.HasPrefix(k.Target, a.Target+"/") {
				return fmt.Errorf("keep %q must be a drive mounted below %s", k.Target, a.Target)
			}
		}
	}
	return nil
}

// ExplainMountError names the attached folder the engine refused and the fix. Docker Desktop reports a folder it
// doesn't share exactly like a missing one, so the fix covers both.
func ExplainMountError(err error, attach []driver.Attach) error {
	msg := err.Error()
	if !strings.Contains(msg, "bind source path does not exist") && !strings.Contains(msg, "is not shared from the host") && !strings.Contains(msg, "mounts denied") {
		return err
	}
	what, matched := "an attached folder", ""
	for _, a := range attach {
		// The longest match wins: /Users/me/app is also a prefix of a missing /Users/me/app2.
		if strings.Contains(msg, a.Host) && len(a.Host) > len(matched) {
			what, matched = fmt.Sprintf("attach.%s (%s)", path.Base(a.Target), a.Host), a.Host
		}
	}
	return fmt.Errorf("%w; fix: Docker can't see %s: check that it exists and, with Docker Desktop, that it is inside a shared folder (Settings > Resources > File sharing; /Users, /Volumes, /private, /tmp and /var/folders by default), then apply the Fleet again", err, what)
}

// BoundFolders reads a staged container's attached folders back from its bind mounts.
func BoundFolders(c container.InspectResponse) []driver.Attach {
	var out []driver.Attach
	if c.HostConfig == nil {
		return nil
	}
	for _, m := range c.HostConfig.Mounts {
		if m.Type == mount.TypeBind {
			// Docker Desktop stores a host path as seen from its VM, under /host_mnt.
			host := m.Source
			if strings.HasPrefix(host, "/host_mnt/") {
				host = strings.TrimPrefix(host, "/host_mnt")
			}
			out = append(out, driver.Attach{Host: host, Target: m.Target})
		}
	}
	return out
}
