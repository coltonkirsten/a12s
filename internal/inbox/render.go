package inbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
)

// NonceLen is the hex length of a Batch nonce (8 random bytes).
const NonceLen = 16

// worstN is the widest message index a Batch can carry: 2,000 Messages is
// the maxMessages ceiling, so four digits.
const worstN = 9999

// RenderTime formats a Message time as it appears in a frame header.
func RenderTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// attrEscape is the §4.8 attribute escaping of the metadata JSON.
var attrEscape = strings.NewReplacer("&", "&amp;", "'", "&apos;", `"`, "&quot;", "<", "&lt;", ">", "&gt;")

// MetaAttr is the escaped compact JSON of metadata, or "" when empty.
func MetaAttr(md map[string]string) string {
	if len(md) == 0 {
		return ""
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(md) // map keys are sorted
	return attrEscape.Replace(strings.TrimSuffix(b.String(), "\n"))
}

// Frame is the part of a Message the render needs.
type Frame struct {
	ID         string
	Sender     string
	Via        string
	CreatedAt  time.Time
	Body       string
	Metadata   map[string]string
	ReplaceKey string
	Superseded int64
}

// RenderFrame renders one Message as message n of a Batch with nonce.
func RenderFrame(n int, nonce string, f Frame) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<msg-%s n="%d" id="%s" from="%s" via="%s" at="%s"`, nonce, n, f.ID, f.Sender, f.Via, RenderTime(f.CreatedAt))
	if f.ReplaceKey != "" {
		fmt.Fprintf(&b, ` key="%s" superseded="%d"`, f.ReplaceKey, f.Superseded)
	}
	if m := MetaAttr(f.Metadata); m != "" {
		b.WriteString(` meta='` + m + `'`)
	}
	b.WriteString(">\n")
	b.WriteString(f.Body)
	b.WriteString("\n</msg-" + nonce + ">\n\n")
	return b.String()
}

// JSONLen is the length of s once JSON-encoded as a string value, without
// the quotes, exactly as the Controller sends it to computerd
// (api.MarshalWire: no HTML escaping).
func JSONLen(s string) int64 {
	b, _ := api.MarshalWire(s)
	return int64(len(b) - 2)
}

// Reserve is the rendered size reserved for a Message at admission: its
// frame with the widest index and a nonce, JSON-escaped as computerd
// receives it (§4.3). The sum of a Batch's reservations bounds its
// rendered messages, whatever position each takes.
//
// A keyed Message reserves the widest superseded count (ten digits), so a
// count carried over on a return (§4.12) never changes its reservation.
func Reserve(f Frame) int64 {
	if f.ReplaceKey != "" {
		f.Superseded = 9999999999
	}
	return JSONLen(RenderFrame(worstN, strings.Repeat("0", NonceLen), f))
}
