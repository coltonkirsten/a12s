package fleet

// Pins are the image IDs the Controller resolved each ref to, kept in its own state so a running Fleet never moves
// to a different image under the same tag. A pin is added the first time a ref is used and never moves; a new ref
// (a new tag) gets a new pin, and so does a ref whose pinned image is gone.
type Pins struct {
	Images   map[string]string `json:"images"`
	Bundles  map[string]string `json:"bundles"`
	Services map[string]string `json:"services,omitempty"`
}

func ServiceKey(name, ref string) string { return name + "@" + ref }

func BundleKey(kind, version string) string { return kind + "@" + version }

func NewPins() *Pins {
	return &Pins{Images: map[string]string{}, Bundles: map[string]string{}, Services: map[string]string{}}
}
