// Package auth authenticates Callers by bearer Credential (ADR-0004). Every route
// except /healthz goes behind Middleware.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/azthrun/dreambig-nas-file-api/internal/audit"
	"github.com/azthrun/dreambig-nas-file-api/internal/config"
)

// Authenticator matches a presented Credential to a configured Caller.
type Authenticator struct {
	callers []caller
}

type caller struct {
	config.Caller
	digest [sha256.Size]byte
}

// New returns an Authenticator for callers.
func New(callers []config.Caller) *Authenticator {
	a := &Authenticator{}
	for _, c := range callers {
		a.callers = append(a.callers, caller{Caller: c, digest: sha256.Sum256([]byte(c.Token))})
	}
	return a
}

// lookup returns the Caller whose token is token. It compares fixed-size SHA-256
// digests in constant time against every Caller, so neither timing nor an early exit
// reveals a token's length, content or position.
func (a *Authenticator) lookup(token string) (config.Caller, bool) {
	d := sha256.Sum256([]byte(token))
	match := -1
	for i := range a.callers {
		if subtle.ConstantTimeCompare(d[:], a.callers[i].digest[:]) == 1 {
			match = i
		}
	}
	if match < 0 {
		return config.Caller{}, false
	}
	return a.callers[match].Caller, true
}

type ctxKey struct{}

// CallerFrom returns the authenticated Caller of a request that passed Middleware.
func CallerFrom(ctx context.Context) (config.Caller, bool) {
	c, ok := ctx.Value(ctxKey{}).(config.Caller)
	return c, ok
}

// Middleware lets a request through to next only with "Authorization: Bearer <token>"
// naming a configured Caller, and records that Caller's name in the audit Record.
// Otherwise it answers 401 with a WWW-Authenticate challenge.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, presented := bearer(r.Header.Get("Authorization"))
		c, ok := a.lookup(token)
		if !presented || !ok {
			challenge := `Bearer realm="nas-file-api"`
			if presented {
				challenge += `, error="invalid_token"`
			}
			w.Header().Set("WWW-Authenticate", challenge)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "missing or invalid Credential"})
			return
		}
		audit.FromContext(r.Context()).SetCaller(c.Name)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, c)))
	})
}

func bearer(h string) (string, bool) {
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}
