package adminauth

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeTailscaled serves a LocalAPI whois on a Unix socket, answering from a
// table keyed by peer address. An address missing from the table gets the
// daemon's real answer for a non-tailnet peer: 404.
func fakeTailscaled(t *testing.T, peers map[string]string) string {
	t.Helper()
	// Not t.TempDir: macOS caps a socket path at 104 bytes and test temp dirs
	// are long enough to blow through it.
	dir, err := os.MkdirTemp("", "ts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/localapi/v0/whois" {
			http.NotFound(w, r)
			return
		}
		body, ok := peers[r.URL.Query().Get("addr")]
		if !ok {
			http.Error(w, "no match for IP:port", http.StatusNotFound)
			return
		}
		w.Write([]byte(body))
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func tsRequest(remote string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/admin", nil)
	r.RemoteAddr = remote
	return r
}

func TestTailscaleIdentifiesAPerson(t *testing.T) {
	sock := fakeTailscaled(t, map[string]string{
		"100.65.0.1:51000": `{"Node":{"Tags":null},"UserProfile":{"ID":42,"LoginName":"ops@github"}}`,
	})
	id, ok := newTailscale(sock).Identify(tsRequest("100.65.0.1:51000"))
	if !ok || id.Email != "ops@github" || id.UserID != "42" {
		t.Fatalf("Identify = %+v, %v; want ops@github / 42", id, ok)
	}
}

func TestTailscaleRefusesTaggedNodes(t *testing.T) {
	// Every server on a tailnet is tagged. A compromised one must not be able
	// to drive the dashboard, so a tag is a refusal even with a login attached.
	sock := fakeTailscaled(t, map[string]string{
		"100.96.0.2:4000": `{"Node":{"Tags":["tag:exe"]},"UserProfile":{"ID":1,"LoginName":"tagged-devices"}}`,
	})
	if id, ok := newTailscale(sock).Identify(tsRequest("100.96.0.2:4000")); ok {
		t.Fatalf("tagged node identified as %+v", id)
	}
}

func TestTailscaleRefusesNonTailnetPeers(t *testing.T) {
	// Loopback is what a local reverse proxy looks like. It must get no
	// identity, or a public route through the proxy reaches /admin.
	sock := fakeTailscaled(t, map[string]string{})
	if id, ok := newTailscale(sock).Identify(tsRequest("127.0.0.1:41234")); ok {
		t.Fatalf("loopback peer identified as %+v", id)
	}
}

func TestTailscaleFailsClosedWithoutTheDaemon(t *testing.T) {
	p := newTailscale(filepath.Join(t.TempDir(), "absent.sock"))
	if id, ok := p.Identify(tsRequest("100.65.0.1:51000")); ok {
		t.Fatalf("identified %+v with no tailscaled to ask", id)
	}
}

func TestTailscaleRefusesGarbageAndEmptyLogins(t *testing.T) {
	sock := fakeTailscaled(t, map[string]string{
		"100.65.0.3:1": `not json`,
		"100.65.0.4:1": `{"Node":{},"UserProfile":{"ID":7,"LoginName":"  "}}`,
	})
	p := newTailscale(sock)
	for _, addr := range []string{"100.65.0.3:1", "100.65.0.4:1"} {
		if id, ok := p.Identify(tsRequest(addr)); ok {
			t.Errorf("%s identified as %+v", addr, id)
		}
	}
}

func TestGuardRefusesAnonymousWhenProviderHasNoLogin(t *testing.T) {
	// A redirect to "" would send the browser back to the page it is on.
	g, err := NewGuard(newTailscale(filepath.Join(t.TempDir(), "absent.sock")), []string{"ops@github"})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	g.Protect(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the protected handler ran for an anonymous request")
	})).ServeHTTP(rec, tsRequest("127.0.0.1:1"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("Location = %q, want no redirect", loc)
	}
}

func TestGuardAdmitsAllowlistedTailnetUser(t *testing.T) {
	sock := fakeTailscaled(t, map[string]string{
		"100.65.0.1:51000": `{"Node":{"Tags":[]},"UserProfile":{"ID":42,"LoginName":"Ops@GitHub"}}`,
	})
	g, err := NewGuard(newTailscale(sock), []string{"ops@github"})
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	g.Protect(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true })).
		ServeHTTP(httptest.NewRecorder(), tsRequest("100.65.0.1:51000"))
	if !ran {
		t.Error("allowlisted tailnet user was refused")
	}
}
