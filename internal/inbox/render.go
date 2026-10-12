package inbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
)

const NonceLen = 16

// worstN has four digits because maxMessages caps a Batch at 2,000.
const worstN = 9999

func RenderTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// attrEscape keeps metadata JSON from closing the single-quoted meta attribute.
var attrEscape = strings.NewReplacer("&", "&amp;", "'", "&apos;", `"`, "&quot;", "<", "&lt;", ">", "&gt;")

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

// JSONLen must match api.MarshalWire (no HTML escaping), since that is what
// computerd receives.
func JSONLen(s string) int64 {
	b, _ := api.MarshalWire(s)
	return int64(len(b) - 2)
}

// Reserve renders with the widest index so the sum of a Batch's reservations
// bounds its rendered size whatever position each Message takes. A keyed
// Message reserves a ten-digit superseded count so a count carried over on a
// return never changes its reservation.
func Reserve(f Frame) int64 {
	if f.ReplaceKey != "" {
		f.Superseded = 9999999999
	}
	return JSONLen(RenderFrame(worstN, strings.Repeat("0", NonceLen), f))
}
