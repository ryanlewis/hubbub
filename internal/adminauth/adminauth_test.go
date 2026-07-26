package adminauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func guard(t *testing.T, allowed ...string) *Guard {
	t.Helper()
	p, err := New("exe-dev")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g, err := NewGuard(p, allowed)
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}
	return g
}

func req(headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/admin", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestAnonymousIsRedirectedToLogin(t *testing.T) {
	g := guard(t, "ops@example.com")
	rec := httptest.NewRecorder()
	g.Protect(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the protected handler ran for an anonymous request")
	})).ServeHTTP(rec, req(nil))

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, loginPath) {
		t.Errorf("Location = %q, want it to start with %q", loc, loginPath)
	}
	if !strings.Contains(loc, "redirect=%2Fadmin") {
		t.Errorf("Location = %q, want it to carry the return path", loc)
	}
}

func TestAllowlistedIdentityPasses(t *testing.T) {
	g := guard(t, "ops@example.com")
	rec := httptest.NewRecorder()
	ran := false
	g.Protect(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true })).
		ServeHTTP(rec, req(map[string]string{headerEmail: "ops@example.com", headerUserID: "usr1"}))

	if !ran {
		t.Error("the protected handler did not run")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// Being logged in as the wrong one of your own accounts is the overwhelmingly
// likely cause of a refusal, and a bare 403 makes that invisible.
func TestRefusedIdentityIsNamed(t *testing.T) {
	g := guard(t, "ops@example.com")
	rec := httptest.NewRecorder()
	g.Protect(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the protected handler ran for a non-allowlisted identity")
	})).ServeHTTP(rec, req(map[string]string{headerEmail: "someone@example.com"}))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "someone@example.com") {
		t.Errorf("body does not name the refused address: %q", rec.Body.String())
	}
}

func TestMatchingIsCaseInsensitiveAndTrimmed(t *testing.T) {
	g := guard(t, "  Ops@Example.COM  ")
	for _, addr := range []string{"ops@example.com", "OPS@EXAMPLE.COM", " ops@example.com "} {
		if !g.Permits(addr) {
			t.Errorf("Permits(%q) = false, want true", addr)
		}
	}
	for _, addr := range []string{"", "opsexample.com", "ops@example.com.evil.com", "evil@example.com"} {
		if g.Permits(addr) {
			t.Errorf("Permits(%q) = true, want false", addr)
		}
	}
}

// Fail closed. A missing allowlist is a half-finished config, not an intent to
// publish key management to the internet.
func TestEmptyAllowlistIsRefused(t *testing.T) {
	p, err := New("exe-dev")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, allowed := range [][]string{nil, {}, {""}, {"   "}} {
		if _, err := NewGuard(p, allowed); err == nil {
			t.Errorf("NewGuard(%q) succeeded, want a refusal", allowed)
		}
	}
	if _, err := NewGuard(nil, []string{"a@b.c"}); err == nil {
		t.Error("NewGuard with no provider succeeded")
	}
}

func TestUnknownProviderIsNamedWithTheKnownOnes(t *testing.T) {
	_, err := New("google-sso")
	if err == nil {
		t.Fatal("New succeeded for an unregistered provider")
	}
	if !strings.Contains(err.Error(), "exe-dev") {
		t.Errorf("err = %v, want it to list the known providers", err)
	}
	if got := Names(); len(got) != 1 || got[0] != "exe-dev" {
		t.Errorf("Names() = %v, want [exe-dev]", got)
	}
}

func TestIdentifyRequiresAnEmail(t *testing.T) {
	p := exeDev{}
	if _, ok := p.Identify(req(nil)); ok {
		t.Error("identified a request with no headers")
	}
	if _, ok := p.Identify(req(map[string]string{headerUserID: "usr1"})); ok {
		t.Error("identified a request carrying only a user id")
	}
	if _, ok := p.Identify(req(map[string]string{headerEmail: "   "})); ok {
		t.Error("identified a request with a blank email")
	}
	id, ok := p.Identify(req(map[string]string{headerEmail: " a@b.c ", headerUserID: " usr1 "}))
	if !ok || id.Email != "a@b.c" || id.UserID != "usr1" {
		t.Errorf("Identify = %+v, %v", id, ok)
	}
}

// The return path is echoed into a URL, so it must not be able to carry the
// visitor to another origin.
func TestLoginURLCannotBeRedirectedOffHost(t *testing.T) {
	p := exeDev{}
	for _, in := range []string{
		"//evil.example.com/",
		"https://evil.example.com/",
		"/admin",
		"admin",
	} {
		got := p.LoginURL(in)
		if !strings.HasPrefix(got, loginPath+"?redirect=") {
			t.Errorf("LoginURL(%q) = %q, want it rooted at the login path", in, got)
		}
		if strings.Contains(got, "evil.example.com/") && !strings.Contains(got, "%2F") {
			t.Errorf("LoginURL(%q) = %q, left an unescaped off-host target", in, got)
		}
	}
}

func TestProviderStatesItsDeploymentAssumption(t *testing.T) {
	p, err := New("exe-dev")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := p.Warning()
	if !strings.Contains(w, headerEmail) || !strings.Contains(w, "proxy") {
		t.Errorf("Warning() = %q, want it to name the header and the proxy it depends on", w)
	}
}

func TestGuardDistinguishesAnonymousFromRefused(t *testing.T) {
	g := guard(t, "ops@example.com")

	if _, auth, perm := g.Identity(req(nil)); auth || perm {
		t.Errorf("anonymous: authenticated=%v permitted=%v, want false/false", auth, perm)
	}
	if _, auth, perm := g.Identity(req(map[string]string{headerEmail: "x@y.z"})); !auth || perm {
		t.Errorf("refused: authenticated=%v permitted=%v, want true/false", auth, perm)
	}
	if _, auth, perm := g.Identity(req(map[string]string{headerEmail: "ops@example.com"})); !auth || !perm {
		t.Errorf("allowed: authenticated=%v permitted=%v, want true/true", auth, perm)
	}
}
