package controller

import (
	"io"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/computerd"
)

// The reconnect cursor moves only past events that decoded: a damaged event,
// or an incomplete one cut off by EOF, is fetched again next time.
func TestReadEventsCursorAdvancesOnlyOnDecodedEvents(t *testing.T) {
	for name, tail := range map[string]string{
		"damaged":    "id: 2\ndata: {not json\n\n",
		"incomplete": "id: 2\ndata: {\"v\":1,\"seq\":2,\"type\":\"turn.fin",
	} {
		c := &Controller{rt: map[string]*runtime{}}
		body := "id: 1\ndata: {\"v\":1,\"seq\":1,\"type\":\"text.delta\",\"data\":{\"text\":\"x\"}}\n\n" + tail
		c.readEvents("scout", computerd.NewStream(io.NopCloser(strings.NewReader(body))))
		if got := c.rt["scout"].lastEventID; got != "1" {
			t.Errorf("%s: cursor = %q, want 1", name, got)
		}
	}
}
