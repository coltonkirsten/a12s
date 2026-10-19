package gce

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/coltonkirsten/a12s/internal/driver"
)

// errNoServices: Capabilities reports Services false, so admission refuses a Fleet with Services before any of
// these is called.
var errNoServices = errors.New("the GCE driver runs no Services")

func (d *Driver) ServiceImage(context.Context, string) (driver.ServiceImage, error) {
	return driver.ServiceImage{}, errNoServices
}

func (d *Driver) StageService(context.Context, driver.ServiceSpec) error { return errNoServices }

func (d *Driver) SetServicePower(context.Context, string, driver.Power, int64) error {
	return errNoServices
}

func (d *Driver) RemoveService(context.Context, string) error { return nil }

func (d *Driver) ServiceExec(context.Context, string, string, []string, io.Reader) (int, string, error) {
	return -1, "", errNoServices
}

func (d *Driver) DialService(context.Context, string, int) (net.Conn, error) {
	return nil, errNoServices
}

func (d *Driver) ServiceLogs(context.Context, string, bool, int) (io.ReadCloser, error) {
	return nil, errNoServices
}

func (d *Driver) ServiceWrite(context.Context, string, string, string, string, string, []byte) error {
	return errNoServices
}
