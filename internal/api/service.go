package api

// PhaseUnhealthy is reported, not restarted. Services have no Busy.
const PhaseUnhealthy Phase = "Unhealthy"

type ServiceChanges struct {
	Created []string `json:"created"`
	Updated []string `json:"updated"` // restarted on a new generation
	Removed []string `json:"removed"` // stopped and deleted; state drive kept
}

type ServiceStatus struct {
	Name  string   `json:"name"`
	Phase Phase    `json:"phase"`
	Error *Failure `json:"error,omitempty"`
	// Image is the reference as written; Pin the image ID it runs.
	Image      string `json:"image"`
	Pin        string `json:"pin,omitempty"`
	Generation int64  `json:"generation"`
	Restarts   int    `json:"restarts"`
	// Health is "ok", or why it failed.
	Health      string   `json:"health,omitempty"`
	HealthSince string   `json:"healthSince,omitempty"`
	Send        []string `json:"send,omitempty"`   // grants.send
	Uses        []string `json:"uses,omitempty"`   // Services it uses
	UsedBy      []string `json:"usedBy,omitempty"` // Computers and Services that use it
	Publish     []string `json:"publish,omitempty"`
	Ports       []string `json:"ports,omitempty"` // "name:port/proto"
	StateDrive  string   `json:"stateDrive,omitempty"`
	EgressMode  string   `json:"egressMode,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
}

type ServicesResponse struct {
	Services []ServiceStatus `json:"services"`
}

// ServicePowerRequest carries credentials because waking re-injects them.
type ServicePowerRequest struct {
	State       Power       `json:"state"`
	Credentials Credentials `json:"credentials,omitempty"`
}

// LoginResponse names the memory-only login container the CLI attaches to.
type LoginResponse struct {
	Container string   `json:"container"`
	User      string   `json:"user"`
	Command   []string `json:"command"`
	Read      []string `json:"read"`
	Produces  string   `json:"produces"`
}
