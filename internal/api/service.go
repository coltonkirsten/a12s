package api

import "encoding/json"

// PhaseUnhealthy is reported, not restarted. Services have no Busy.
const PhaseUnhealthy Phase = "Unhealthy"

type ServiceChanges struct {
	Created []string `json:"created"`
	Updated []string `json:"updated"` // restarted on a new generation
	Removed []string `json:"removed"` // stopped and deleted; state drive kept
}

type ServiceStatus struct {
	Name    string   `json:"name"`
	Phase   Phase    `json:"phase"`
	Error   *Failure `json:"error,omitempty"`
	Waiting *Waiting `json:"waiting,omitempty"`
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
	Ports       []string `json:"ports,omitempty"`  // "name:port/proto"
	StateDrive  string   `json:"stateDrive,omitempty"`
	EgressMode  string   `json:"egressMode,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
	// Spec is the Service as written in the Fleet.
	Spec json.RawMessage `json:"spec,omitempty"`
}

type ServicesResponse struct {
	Services []ServiceStatus `json:"services"`
}

// ServicePowerRequest carries credentials because waking re-injects them.
type ServicePowerRequest struct {
	State       Power       `json:"state"`
	Credentials Credentials `json:"credentials,omitempty"`
}
