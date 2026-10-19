package gce

import (
	"testing"

	"github.com/coltonkirsten/a12s/internal/driver/drivertest"
)

func TestConformance(t *testing.T) {
	h := newHarness(t)
	drivertest.Run(t, drivertest.Setup{Driver: h.d, Spec: testSpec})
}
