package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// The browser surfaces are three separate pages, and the thing that makes them
// one site is a bar that appears on all of them. These tests pin the two
// properties that are easy to break by editing one page: the bar is on every
// page, and it never offers a link the visitor would be bounced from.

const adminLinkHTML = `href="/admin"`

// TestEveryPageCarriesTheSiteNav. A page that renders without the bar is a dead
// end — the reader's only way on is to type a path they would have to already
// know.
func TestEveryPageCarriesTheSiteNav(t *testing.T) {
	s := landingServer(t)

	for _, path := range []string{"/", "/index.html", "/docs"} {
		body := fetch(t, s, path, nil).Body.String()
		if !strings.Contains(body, `class="sitenav"`) {
			t.Errorf("GET %s has no site nav", path)
		}
		for _, link := range []string{`href="/"`, `href="/docs"`} {
			if !strings.Contains(body, link) {
				t.Errorf("GET %s does not link %s", path, link)
			}
		}
	}
}

// TestNavMarksThePageYouAreOn: three near-identical links with no indication of
// which one you already followed is worse than no bar at all.
func TestNavMarksThePageYouAreOn(t *testing.T) {
	s, _, _ := adminServer(t)

	for path, want := range map[string]string{
		"/":      `<a href="/" aria-current="page"`,
		"/docs":  `<a href="/docs" aria-current="page"`,
		"/admin": `<a href="/admin" aria-current="page"`,
	} {
		if body := adminGet(t, s, path, adminEmail).Body.String(); !strings.Contains(body, want) {
			t.Errorf("GET %s does not mark itself as the current page", path)
		}
	}
}

// TestNavOffersNoDeadAdminLink. A hub with no [admin] block does not register
// the route at all, so a link to it would 404 — the one failure a navigation
// bar has no excuse for.
func TestNavOffersNoDeadAdminLink(t *testing.T) {
	s := landingServer(t)
	if s.Admin != nil {
		t.Fatal("this server is supposed to have no dashboard")
	}

	for _, path := range []string{"/", "/docs"} {
		if strings.Contains(fetch(t, s, path, nil).Body.String(), adminLinkHTML) {
			t.Errorf("GET %s links a dashboard this hub does not serve", path)
		}
	}
	// The other half of the claim: it really would be a dead link.
	if rec := fetch(t, s, "/admin", nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET /admin = %d, want 404 without an [admin] block", rec.Code)
	}
}

// TestNavOffersAdminOnlyToAnAdmin. The landing page and the docs are readable
// by anyone who can reach the hub, so the dashboard link is decided per visitor
// rather than per deployment: showing it to a reader who would be bounced is
// both a dead end for them and an advertisement to everyone else.
func TestNavOffersAdminOnlyToAnAdmin(t *testing.T) {
	s, _, _ := adminServer(t)

	for _, path := range []string{"/", "/docs"} {
		for _, email := range []string{"", "someone@example.com"} {
			rec := adminGet(t, s, path, email)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s as %q = %d, want 200: these pages are public", path, email, rec.Code)
			}
			if strings.Contains(rec.Body.String(), adminLinkHTML) {
				t.Errorf("GET %s offers the dashboard to %q", path, email)
			}
		}
		if !strings.Contains(adminGet(t, s, path, adminEmail).Body.String(), adminLinkHTML) {
			t.Errorf("GET %s does not offer the dashboard to the operator", path)
		}
	}
}

// TestAdminPageLinksBackOut. The dashboard is the page an operator is most
// likely to arrive at directly, from a bookmark.
func TestAdminPageLinksBackOut(t *testing.T) {
	s, _, _ := adminServer(t)

	body := adminGet(t, s, "/admin", adminEmail).Body.String()
	for _, link := range []string{`href="/"`, `href="/docs"`} {
		if !strings.Contains(body, link) {
			t.Errorf("the dashboard does not link %s", link)
		}
	}
}
