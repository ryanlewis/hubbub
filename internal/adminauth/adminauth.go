// Package adminauth gates the admin dashboard behind an identity provider.
//
// The provider is a seam rather than a hardcoded integration: hubbub's core is
// platform-agnostic, and "who is allowed to change key permissions" is exactly
// the sort of question whose answer is a property of where the hub is
// deployed. exe-dev is the first — and so far only — provider; a second one is
// a file in this package, not a change to any handler.
//
// There is deliberately no rolled login here. No passwords, no sessions, no
// cookies, no reset flow: the deployment already has an identity system in
// front of it, and re-implementing one badly is the failure this design avoids.
package adminauth

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Identity is who the provider says is making a request.
type Identity struct {
	Email  string
	UserID string
}

// Provider resolves a request to an identity, and knows where to send someone
// who does not yet have one.
type Provider interface {
	// Identify returns the caller's identity, or false if the request carries
	// none. It must never fabricate one.
	Identify(r *http.Request) (Identity, bool)
	// LoginURL is where an anonymous browser is sent to acquire an identity.
	// returnPath is where it should land afterwards.
	LoginURL(returnPath string) string
	// Warning is a startup line describing what the provider assumes about its
	// deployment, or "" if it assumes nothing. Trust in a header is only ever
	// valid under conditions the binary cannot check for itself, so it says so
	// out loud rather than leaving the assumption implicit.
	Warning() string
}

type factory func() Provider

var registry = map[string]factory{}

// Register adds a provider under a config name, mirroring adapter.Register.
// Called from init(), so the map needs no lock.
func Register(name string, f factory) { registry[name] = f }

// New builds the provider named in config.
func New(name string) (Provider, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown admin auth provider %q (known: %s)", name, strings.Join(Names(), ", "))
	}
	return f(), nil
}

// Names lists the registered provider names, sorted.
func Names() []string {
	ns := make([]string, 0, len(registry))
	for n := range registry {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	return ns
}

// Guard admits only allowlisted identities.
type Guard struct {
	Provider Provider
	// allowed is lower-cased for comparison; addresses are matched whole.
	allowed map[string]struct{}
	// Denied, if set, renders the 403 body. Left nil in tests.
	Denied func(w http.ResponseWriter, r *http.Request, email string)
}

// NewGuard builds a Guard over an allowlist.
//
// An empty allowlist is an error, not "allow everyone". A missing allowlist is
// overwhelmingly a half-finished config rather than an intent to publish key
// management to the internet, and this codebase already fails closed on the
// same shape elsewhere — an explicit "channels": [] is a 400, never a
// fall-through to every channel.
func NewGuard(p Provider, allowed []string) (*Guard, error) {
	if p == nil {
		return nil, fmt.Errorf("admin auth provider is required")
	}
	set := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		set[a] = struct{}{}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("admin allowed_emails is empty; refusing to start rather than admit everyone")
	}
	return &Guard{Provider: p, allowed: set}, nil
}

// Permits reports whether an address is on the allowlist. Matching is
// case-insensitive on the whole address: mail addresses are not case-sensitive
// in the part that matters here, and an operator who types their address with
// a capital should not be locked out of their own hub.
func (g *Guard) Permits(email string) bool {
	_, ok := g.allowed[strings.ToLower(strings.TrimSpace(email))]
	return ok
}

// Identity returns the authenticated, allowlisted identity for a request.
// The second result distinguishes "nobody is logged in" (redirect them) from
// "somebody is logged in but not permitted" (tell them who they are).
func (g *Guard) Identity(r *http.Request) (id Identity, authenticated, permitted bool) {
	id, authenticated = g.Provider.Identify(r)
	if !authenticated {
		return Identity{}, false, false
	}
	return id, true, g.Permits(id.Email)
}

// Protect wraps a handler so only allowlisted identities reach it.
func (g *Guard) Protect(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, authenticated, permitted := g.Identity(r)
		switch {
		case !authenticated:
			// A redirect, not a 401: the visitor is a browser that can go and
			// get an identity. Only the path is echoed back into the login
			// URL — never the query, which is where a hostile link would put
			// its payload.
			http.Redirect(w, r, g.Provider.LoginURL(r.URL.EscapedPath()), http.StatusFound)
		case !permitted:
			// Name the address that was refused. A blank wall here is
			// genuinely hard to diagnose: the usual cause is being logged in
			// as the wrong one of your own accounts, and you cannot tell that
			// from a bare 403.
			if g.Denied != nil {
				g.Denied(w, r, id.Email)
				return
			}
			http.Error(w, fmt.Sprintf("%s is not permitted to administer this hub", id.Email), http.StatusForbidden)
		default:
			h.ServeHTTP(w, r)
		}
	})
}
