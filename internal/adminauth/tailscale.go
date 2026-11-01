package adminauth

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func init() {
	Register("tailscale", func() Provider { return newTailscale(tailscaledSocket) })
}

// tailscaledSocket is where tailscaled serves its LocalAPI on Linux. It is a
// convention of the tailscale packages rather than something hubbub can be
// told, so it is fixed here; a host that moved it can symlink it back.
const tailscaledSocket = "/var/run/tailscale/tailscaled.sock"

// whoisTimeout bounds the LocalAPI lookup that every /admin request makes. The
// daemon answers from memory in well under a millisecond; a lookup that takes
// longer than this is a wedged daemon, and an admin page that hangs on it is
// worse than one that refuses.
const whoisTimeout = 2 * time.Second

// tailscale identifies a request by asking tailscaled who owns the peer
// address it came from.
//
// Unlike exe-dev it trusts no header at all. The identity comes from the
// WireGuard key that carried the packet, so it cannot be forged by a client
// and survives being reached directly — the property exe-dev lacks off its
// proxy. The trade is reach: only a request whose TCP peer is a tailnet node
// has an identity. Anything relayed by a local reverse proxy arrives from
// loopback, gets no answer from whois, and is refused, which is what keeps
// /admin off a public route even if the proxy forgets to block it.
type tailscale struct {
	client *http.Client
}

func newTailscale(socket string) tailscale {
	return tailscale{client: &http.Client{
		Timeout: whoisTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}}
}

// whois is the subset of the LocalAPI's apitype.WhoIsResponse used here.
type whois struct {
	Node struct {
		Tags []string `json:"Tags"`
	} `json:"Node"`
	UserProfile struct {
		ID        int64  `json:"ID"`
		LoginName string `json:"LoginName"`
	} `json:"UserProfile"`
}

func (t tailscale) Identify(r *http.Request) (Identity, bool) {
	q := url.Values{"addr": {r.RemoteAddr}}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://local-tailscaled.sock/localapi/v0/whois?"+q.Encode(), nil)
	if err != nil {
		return Identity{}, false
	}
	// tailscaled ignores this on its Unix socket and demands it over TCP; it
	// costs nothing to send and keeps the request valid either way.
	req.Header.Set("Sec-Tailscale", "localapi")
	resp, err := t.client.Do(req)
	if err != nil {
		return Identity{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 404 is "no tailnet node owns that address" — loopback, the LAN, a
		// proxy. It is the expected answer for every non-tailnet request.
		return Identity{}, false
	}
	var w whois
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&w); err != nil {
		return Identity{}, false
	}
	// A tagged node is a machine, not a person: its UserProfile is the
	// placeholder "tagged-devices". Every server on the tailnet is tagged, so
	// admitting them would hand the dashboard to whichever box gets
	// compromised first.
	if len(w.Node.Tags) > 0 {
		return Identity{}, false
	}
	login := strings.TrimSpace(w.UserProfile.LoginName)
	if login == "" {
		return Identity{}, false
	}
	return Identity{Email: login, UserID: strconv.FormatInt(w.UserProfile.ID, 10)}, true
}

// LoginURL is empty: there is no login to send a browser to. Joining the
// tailnet is the login, and it happens outside any page this hub can serve.
func (tailscale) LoginURL(string) string { return "" }

func (tailscale) Warning() string {
	return "admin auth asks tailscaled who owns each request's peer address, so /admin answers only to people " +
		"reaching this host over the tailnet; allowed_emails holds Tailscale login names (e.g. user@github), " +
		"which are not always email addresses"
}
