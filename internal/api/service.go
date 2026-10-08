package api

// PhaseUnhealthy is a Service whose health check stopped passing: reported,
// not restarted (enhancements §5.2). Services have no Busy.
const PhaseUnhealthy Phase = "Unhealthy"

// ServiceChanges lists what applying the Fleet did to Services.
type ServiceChanges struct {
	Created []string `json:"created"`
	Updated []string `json:"updated"` // restarted on a new generation
	Removed []string `json:"removed"` // stopped and deleted; state drive kept
}

// ServiceStatus is one row of `a12s service ls` and `a12s status`.
type ServiceStatus struct {
	Name  string   `json:"name"`
	Phase Phase    `json:"phase"`
	Error *Failure `json:"error,omitempty"`
	// Image is the reference as written; Pin the image ID it runs.
	Image      string `json:"image"`
	Pin        string `json:"pin,omitempty"`
	Generation int64  `json:"generation"`
	Restarts   int    `json:"restarts"`
	// Health is the last health result ("ok", or why it failed) and since when.
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

// ServicesResponse is GET /v1/services.
type ServicesResponse struct {
	Services []ServiceStatus `json:"services"`
}

// ServicePowerRequest is POST /v1/services/{s}/power. Waking re-injects
// credentials, so `a12s wake` sends them.
type ServicePowerRequest struct {
	State       Power       `json:"state"`
	Credentials Credentials `json:"credentials,omitempty"`
}

// LoginResponse is POST /v1/services/{s}/login: the memory-only login
// container the CLI attaches to (enhancements §5.5).
type LoginResponse struct {
	Container string   `json:"container"`
	User      string   `json:"user"`
	Command   []string `json:"command"`
	Read      []string `json:"read"`
	Produces  string   `json:"produces"`
}
