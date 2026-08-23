package s3

import (
	"context"
	"errors"
	"net/http"

	"github.com/Kodiqa-Solutions/VaultS3/internal/iam"
)

// Authorization normally happens once in the router, before a handler runs,
// against the action implied by the method and path. That works for a request
// that names one object, but DeleteObjects names many in its body, and each
// entry may carry its own versionId. A single up-front check cannot express
// that, so the route required s3:* instead: fail-closed, but it also meant a
// user holding s3:DeleteObject could not batch delete at all.
//
// These carry the authenticated identity and the request context IAM conditions
// evaluate against, so a handler that parses a body can authorize each entry
// individually with the same evaluator the router uses.

// errNoEntryAuthorizer means the identity never reached the handler. Treated as
// a denial so a wiring mistake cannot authorize everything.
var errNoEntryAuthorizer = errors.New("access denied: no authenticated identity on the request")

type ctxKey int

const (
	identityKey ctxKey = iota
	reqCtxKey
)

// withIdentity returns a request carrying its authenticated identity and the
// condition context, for handlers that must authorize per entry.
func withIdentity(r *http.Request, id *iam.Identity, reqCtx map[string]string) *http.Request {
	ctx := context.WithValue(r.Context(), identityKey, id)
	ctx = context.WithValue(ctx, reqCtxKey, reqCtx)
	return r.WithContext(ctx)
}

func identityFrom(r *http.Request) (*iam.Identity, map[string]string) {
	id, _ := r.Context().Value(identityKey).(*iam.Identity)
	reqCtx, _ := r.Context().Value(reqCtxKey).(map[string]string)
	return id, reqCtx
}

// authorizeEntry checks one entry of a multi-object request. It fails closed:
// if the identity never reached the context, the answer is no, so a wiring
// mistake denies rather than silently authorizing everything.
func (h *ObjectHandler) authorizeEntry(r *http.Request, action, resource string) error {
	if h.auth == nil {
		return errNoEntryAuthorizer
	}
	id, reqCtx := identityFrom(r)
	if id == nil {
		return errNoEntryAuthorizer
	}
	if id.IsAdmin {
		return nil
	}
	return h.auth.AuthorizeWithContext(id, action, resource, reqCtx)
}
