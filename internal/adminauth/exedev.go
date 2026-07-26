package adminauth

import (
	"net/http"
	"net/url"
	"strings"
)

func init() { Register("exe-dev", func() Provider { return exeDev{} }) }

// Headers the exe.dev proxy injects for an authenticated user.
const (
	headerEmail  = "X-ExeDev-Email"
	headerUserID = "X-ExeDev-UserID"
)

// loginPath is the proxy's own login endpoint. It is served by the proxy, not
// by this process, so hubbub never sees a request for it.
const loginPath = "/__exe.dev/login"

// exeDev identifies users from the headers the exe.dev proxy injects.
//
// The proxy strips any client-supplied X-ExeDev-* header before forwarding,
// which is the entire basis for trusting these two. That is a property of the
// proxy, not of this code — see Warning.
type exeDev struct{}

func (exeDev) Identify(r *http.Request) (Identity, bool) {
	email := strings.TrimSpace(r.Header.Get(headerEmail))
	if email == "" {
		return Identity{}, false
	}
	return Identity{Email: email, UserID: strings.TrimSpace(r.Header.Get(headerUserID))}, true
}

// LoginURL bounces an anonymous browser through the proxy's login flow. On a
// public proxy the request reaches this process unauthenticated, so making
// that round trip happen is the application's job.
//
// The redirect target is host-relative and carried as a query parameter, so
// there is no way to point it at another origin.
func (exeDev) LoginURL(returnPath string) string {
	if !strings.HasPrefix(returnPath, "/") {
		returnPath = "/" + returnPath
	}
	return loginPath + "?redirect=" + url.QueryEscape(returnPath)
}

func (exeDev) Warning() string {
	return "admin auth trusts the " + headerEmail + " header, which is only safe behind the exe.dev proxy: " +
		"reached directly — a local run, a bypassed port — that header is whatever the client says it is"
}
