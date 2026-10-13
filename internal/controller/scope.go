package controller

import (
	"net/http"

	"github.com/coltonkirsten/a12s/internal/api"
)

// sees reports whether the caller may see a name in Fleet-wide reads.
func (k caller) sees(name string) bool { return true }

// mayCredential reports whether the caller may set or remove a credential.
func (k caller) mayCredential(name string) bool { return k.owner() }

func outOfPrefix(k caller, what string) error {
	return &httpError{status: http.StatusForbidden, body: api.Error{Code: api.ErrOutOfPrefix,
		Message: k.principal + " may not touch " + what, Fix: "use the owner token"}}
}
