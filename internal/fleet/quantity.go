package fleet

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Bytes is a size in bytes. In YAML and JSON it is written as a plain integer
// or with a binary unit suffix: Ki, Mi, Gi or Ti (for example "10Gi").
type Bytes int64

var binaryUnits = []struct {
	suffix string
	factor int64
}{
	{"Ti", 1 << 40},
	{"Gi", 1 << 30},
	{"Mi", 1 << 20},
	{"Ki", 1 << 10},
}

// ParseBytes parses "10Gi", "512Mi", "1.5Gi" or "1048576".
func ParseBytes(s string) (Bytes, error) {
	s = strings.TrimSpace(s)
	num, factor := s, int64(1)
	for _, u := range binaryUnits {
		if strings.HasSuffix(s, u.suffix) {
			num, factor = strings.TrimSuffix(s, u.suffix), u.factor
			break
		}
	}
	if num == "" {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if n, err := strconv.ParseInt(num, 10, 64); err == nil {
		if n < 0 || (n > 0 && factor > math.MaxInt64/n) {
			return 0, fmt.Errorf("invalid size %q: out of range", s)
		}
		return Bytes(n * factor), nil
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, fmt.Errorf("invalid size %q: want a number with an optional Ki, Mi, Gi or Ti suffix", s)
	}
	v := f * float64(factor)
	if v >= math.MaxInt64 {
		return 0, fmt.Errorf("invalid size %q: out of range", s)
	}
	return Bytes(math.Round(v)), nil
}

// String formats b with the largest binary unit that divides it exactly.
func (b Bytes) String() string {
	for _, u := range binaryUnits {
		if b != 0 && int64(b)%u.factor == 0 {
			return strconv.FormatInt(int64(b)/u.factor, 10) + u.suffix
		}
	}
	return strconv.FormatInt(int64(b), 10)
}

// MarshalText implements encoding.TextMarshaler.
func (b Bytes) MarshalText() ([]byte, error) { return []byte(b.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (b *Bytes) UnmarshalText(text []byte) error {
	v, err := ParseBytes(string(text))
	if err != nil {
		return err
	}
	*b = v
	return nil
}

// CPU is a CPU quantity in millicores. In YAML and JSON it is written as a
// number of cores ("1", "0.5") or in millicores ("500m").
type CPU int64

// ParseCPU parses "2", "0.5" or "500m".
func ParseCPU(s string) (CPU, error) {
	s = strings.TrimSpace(s)
	if m, ok := strings.CutSuffix(s, "m"); ok {
		n, err := strconv.ParseInt(m, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid cpu %q", s)
		}
		return CPU(n), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) || f > 1e9 {
		return 0, fmt.Errorf("invalid cpu %q: want cores (\"1\", \"0.5\") or millicores (\"500m\")", s)
	}
	return CPU(math.Round(f * 1000)), nil
}

// Millis returns the quantity in millicores.
func (c CPU) Millis() int64 { return int64(c) }

// String formats c as whole cores when exact, else millicores.
func (c CPU) String() string {
	if c%1000 == 0 {
		return strconv.FormatInt(int64(c)/1000, 10)
	}
	return strconv.FormatInt(int64(c), 10) + "m"
}

// MarshalText implements encoding.TextMarshaler.
func (c CPU) MarshalText() ([]byte, error) { return []byte(c.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (c *CPU) UnmarshalText(text []byte) error {
	v, err := ParseCPU(string(text))
	if err != nil {
		return err
	}
	*c = v
	return nil
}

// UnmarshalJSON accepts a JSON number (cores) as well as a string, as YAML does.
func (c *CPU) UnmarshalJSON(b []byte) error {
	t := strings.Trim(string(b), `"`)
	return c.UnmarshalText([]byte(t))
}

// UnmarshalJSON accepts a JSON number (bytes) as well as a string ("512Mi").
func (b *Bytes) UnmarshalJSON(data []byte) error {
	t := strings.Trim(string(data), `"`)
	return b.UnmarshalText([]byte(t))
}
