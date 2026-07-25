package adapter

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html/template"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ryanlewis/hubbub/internal/notify"
)

// smtp delivers over plain SMTP submission, which is what makes an HTML
// notification possible at all: the gateway this replaces sends text/plain and
// nothing else, so no amount of formatting upstream survives the trip.
//
// Written against net/smtp rather than a mailer library, on purpose. What a
// library adds here is API sugar over four stdlib packages that already do the
// work — net/smtp for the conversation, mime/multipart for the body,
// mime/quotedprintable for the encoding, mime for the header words — and the
// module graph stays at two entries.
//
// The one thing net/smtp does not give us is context: smtp.SendMail dials,
// talks and returns with no way to cancel, so the per-attempt timeout the
// worker sets would be enforced by nothing. Hence the hand-driven client
// below, over a conn this adapter dialled itself.

const (
	tlsSTARTTLS = "starttls"
	tlsImplicit = "implicit"
	tlsNone     = "none"

	smtpSubmissionPort = 587
	smtpImplicitPort   = 465

	defaultRateLimitCooldown = 15 * time.Minute
)

func init() {
	Register("smtp", func(id string, decode Decode) (Adapter, error) {
		return newSMTP(id, decode)
	})
}

type smtpConfig struct {
	Host     string   `toml:"host"`
	Port     int      `toml:"port"`
	Username string   `toml:"username"`
	Password string   `toml:"password"`
	From     string   `toml:"from"`
	FromName string   `toml:"from_name"`
	To       []string `toml:"to"`
	TLS      string   `toml:"tls"`
	HELO     string   `toml:"helo"`
	// RateLimitCooldown is how long to hold a channel that answered a 4xx
	// naming a send limit. SMTP has no Retry-After, so unlike the ntfy adapter
	// there is nothing to read it from — it is a setting because a provider's
	// idea of "slow down" ranges from seconds to a day.
	RateLimitCooldown string `toml:"rate_limit_cooldown"`
}

type smtpAdapter struct {
	id         string
	addr       string
	host       string
	helo       string
	mode       string
	auth       smtp.Auth
	fromAddr   string
	fromHeader string
	rcpt       []string
	toHeader   string
	cooldown   time.Duration

	// tlsConfig overrides what clientTLS derives from host. Unexported and
	// unset by the factory: it exists so the tests can trust a throwaway CA
	// without an "insecure" config key that a real deployment could then set.
	tlsConfig *tls.Config
}

func newSMTP(id string, decode Decode) (*smtpAdapter, error) {
	var c smtpConfig
	if err := decode(&c); err != nil {
		return nil, fmt.Errorf("channel %q (smtp): %w", id, err)
	}

	fail := func(format string, args ...any) error {
		return fmt.Errorf("channel %q (smtp): "+format, append([]any{id}, args...)...)
	}

	if c.Host == "" {
		return nil, fail("host is required")
	}
	mode := strings.ToLower(strings.TrimSpace(c.TLS))
	if mode == "" {
		// 465 is the implicit-TLS submission port and 587 the STARTTLS one, so
		// the port already says which handshake the operator meant.
		if c.Port == smtpImplicitPort {
			mode = tlsImplicit
		} else {
			mode = tlsSTARTTLS
		}
	}
	switch mode {
	case tlsSTARTTLS, tlsImplicit, tlsNone:
	default:
		return nil, fail("tls = %q (starttls|implicit|none)", c.TLS)
	}
	if mode == tlsNone && c.Password != "" {
		// The failure this prevents is silent and total: PlainAuth declines to
		// transmit over a bare connection unless it is talking to localhost, so
		// the config would look configured and every send would fail on auth —
		// and where it *did* work, the app password would be on the wire.
		return nil, fail(`tls = "none" cannot carry a password; use starttls, or drop the password for a local relay`)
	}

	if c.Port == 0 {
		if mode == tlsImplicit {
			c.Port = smtpImplicitPort
		} else {
			c.Port = smtpSubmissionPort
		}
	}
	if c.Port < 1 || c.Port > 65535 {
		return nil, fail("port %d out of range", c.Port)
	}

	if c.From == "" {
		return nil, fail("from is required")
	}
	from, err := mail.ParseAddress(c.From)
	if err != nil {
		return nil, fail("from %q: %v", c.From, err)
	}
	if len(c.To) == 0 {
		return nil, fail("to is required (at least one recipient)")
	}
	rcpt := make([]string, 0, len(c.To))
	headers := make([]string, 0, len(c.To))
	for _, raw := range c.To {
		to, err := mail.ParseAddress(raw)
		if err != nil {
			return nil, fail("to %q: %v", raw, err)
		}
		rcpt = append(rcpt, to.Address)
		headers = append(headers, to.String())
	}

	cooldown := defaultRateLimitCooldown
	if c.RateLimitCooldown != "" {
		d, err := time.ParseDuration(c.RateLimitCooldown)
		if err != nil || d <= 0 {
			return nil, fail("rate_limit_cooldown %q: want a positive duration like \"15m\"", c.RateLimitCooldown)
		}
		cooldown = d
	}

	a := &smtpAdapter{
		id:   id,
		addr: net.JoinHostPort(c.Host, strconv.Itoa(c.Port)),
		host: c.Host,
		// Servers vary on whether they care, and the ones that do reject an
		// EHLO name that is empty rather than uninformative.
		helo:       cmp.Or(c.HELO, "localhost"),
		mode:       mode,
		fromAddr:   from.Address,
		fromHeader: (&mail.Address{Name: c.FromName, Address: from.Address}).String(),
		rcpt:       rcpt,
		toHeader:   strings.Join(headers, ", "),
		cooldown:   cooldown,
	}
	if c.Password != "" {
		user := cmp.Or(c.Username, from.Address)
		// PLAIN only. It is what the providers this targets advertise, and the
		// alternative worth having (CRAM-MD5) is weaker than PLAIN-inside-TLS,
		// not stronger.
		a.auth = smtp.PlainAuth("", user, c.Password, c.Host)
	}
	return a, nil
}

func (a *smtpAdapter) Send(ctx context.Context, n notify.Notification) error {
	msg, err := a.compose(n)
	if err != nil {
		// Composition failing is a bug in this adapter, not a bad message:
		// retrying re-runs the same code over the same bytes forever.
		return Permanent("smtp: compose: %v", err)
	}
	return a.deliver(ctx, msg)
}

// deliver runs one SMTP submission. Every step's error goes through classify,
// so the worker's retry decision comes from the server's reply code rather
// than from which command happened to fail.
func (a *smtpAdapter) deliver(ctx context.Context, msg []byte) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", a.addr)
	if err != nil {
		return Retryable("smtp: dial %s: %v", a.addr, err)
	}
	// Two mechanisms because they cover different failures: the deadline ends
	// a server that stops responding mid-conversation, and closing the socket
	// on cancellation ends a shutdown or config reload that arrives while we
	// are blocked in a read. `raw` is captured deliberately — the TLS
	// handshake reassigns conn, and closing the socket underneath is what we
	// want anyway.
	raw := conn
	defer raw.Close()
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}

	if a.mode == tlsImplicit {
		tconn := tls.Client(conn, a.clientTLS())
		if err := tconn.HandshakeContext(ctx); err != nil {
			return a.classify(ctx, "tls handshake", err)
		}
		conn = tconn
	}

	c, err := smtp.NewClient(conn, a.host)
	if err != nil {
		return a.classify(ctx, "greeting", err)
	}
	defer c.Close()

	if err := c.Hello(a.helo); err != nil {
		return a.classify(ctx, "ehlo", err)
	}
	if a.mode == tlsSTARTTLS {
		// Refused rather than downgraded. A submission server that has stopped
		// advertising STARTTLS is either misconfigured or being stripped by
		// something in the middle, and continuing would put the app password
		// on the wire in both cases.
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return Retryable("smtp: %s does not offer STARTTLS", a.host)
		}
		if err := c.StartTLS(a.clientTLS()); err != nil {
			return a.classify(ctx, "starttls", err)
		}
	}
	if a.auth != nil {
		if err := c.Auth(a.auth); err != nil {
			return a.classify(ctx, "auth", err)
		}
	}

	if err := c.Mail(a.fromAddr); err != nil {
		return a.classify(ctx, "mail from", err)
	}
	for _, to := range a.rcpt {
		if err := c.Rcpt(to); err != nil {
			return a.classify(ctx, "rcpt to", err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return a.classify(ctx, "data", err)
	}
	if _, err := w.Write(msg); err != nil {
		return a.classify(ctx, "write body", err)
	}
	// Close is where the server's verdict on the message arrives — the 250 for
	// the whole submission, or the rejection. It is the line that decides
	// whether this send happened.
	if err := w.Close(); err != nil {
		return a.classify(ctx, "body", err)
	}
	// A QUIT that fails after the server accepted the message is not a
	// delivery failure, and reporting one would re-queue a mail already in the
	// recipient's inbox.
	_ = c.Quit()
	return nil
}

func (a *smtpAdapter) clientTLS() *tls.Config {
	if a.tlsConfig != nil {
		return a.tlsConfig.Clone()
	}
	return &tls.Config{ServerName: a.host, MinVersion: tls.VersionTLS12}
}

// throttleHints are the phrases providers put in a 4xx when the reply means
// "you are sending too much" rather than "try again shortly". The distinction
// matters because the two want different waits: the generic retry backoff tops
// out at five minutes, which against a send quota is just a busier way to keep
// being refused.
var throttleHints = []string{
	"too many", "rate limit", "ratelimit", "throttl",
	"quota", "try again later", "slow down", "sending limit",
}

// classify maps a failed SMTP step onto the worker's three retry kinds.
//
// The reply code is the contract: 4xx is a transient refusal the RFC says to
// retry, 5xx is a refusal that will be repeated for the same message. Anything
// that is not a protocol reply at all — a dropped connection, a TLS failure, a
// deadline — is the network, and the network is retryable.
func (a *smtpAdapter) classify(ctx context.Context, stage string, err error) error {
	// A cancelled attempt is not a verdict on the message. The worker discards
	// it either way; saying retryable keeps it from ever reading as permanent.
	if ctx.Err() != nil {
		return Retryable("smtp: %s: %v", stage, ctx.Err())
	}

	var reply *textproto.Error
	if errors.As(err, &reply) {
		text := strings.ToLower(reply.Msg)
		switch {
		case reply.Code >= 400 && reply.Code < 500:
			for _, hint := range throttleHints {
				if strings.Contains(text, hint) {
					return RateLimited(time.Now().Add(a.cooldown),
						"smtp: %s: %d %s (holding %s)", stage, reply.Code, reply.Msg, a.cooldown)
				}
			}
			return Retryable("smtp: %s: %d %s", stage, reply.Code, reply.Msg)
		case reply.Code >= 500:
			return Permanent("smtp: %s: %d %s", stage, reply.Code, reply.Msg)
		}
	}
	return Retryable("smtp: %s: %v", stage, err)
}

// --- message construction ---------------------------------------------------

var smtpPriorityHeaders = map[notify.Priority]struct{ importance, xPriority string }{
	notify.PriorityUrgent:  {"High", "1"},
	notify.PriorityHigh:    {"High", "2"},
	notify.PriorityLow:     {"Low", "5"},
	notify.PriorityDefault: {},
}

// compose builds the whole RFC 5322 message.
//
// Deterministic in the notification: the same message re-composed on a retry
// carries the same Date and Message-ID. Delivery is at-least-once, so that is
// load-bearing — a duplicate submission arrives with an id the client has
// already filed, and most of them collapse it rather than showing it twice.
func (a *smtpAdapter) compose(n notify.Notification) ([]byte, error) {
	html, err := a.htmlBody(n)
	if err != nil {
		return nil, err
	}

	var parts bytes.Buffer
	mw := multipart.NewWriter(&parts)
	// Plain first. multipart/alternative is ordered worst-to-best, so a client
	// that can render both takes the last part it understands — and a client
	// that can't render HTML at all still gets the message rather than a
	// screenful of markup.
	if err := writeMIMEPart(mw, "text/plain; charset=utf-8", []byte(plainBody(n))); err != nil {
		return nil, err
	}
	if err := writeMIMEPart(mw, "text/html; charset=utf-8", html); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	sent := n.CreatedAt
	if sent.IsZero() {
		sent = time.Now()
	}

	var msg bytes.Buffer
	header := func(k, v string) { fmt.Fprintf(&msg, "%s: %s\r\n", k, v) }
	header("From", a.fromHeader)
	header("To", a.toHeader)
	// QEncoding encodes anything outside printable ASCII, which is also what
	// makes header injection impossible here: a CR or LF is below 0x20, so a
	// title carrying one comes out as =0D=0A inside an encoded word instead of
	// starting a header of the caller's choosing. Titles are already stripped
	// of control characters at ingest; this is the second lock on the door
	// that matters most.
	header("Subject", mime.QEncoding.Encode("utf-8", subjectFor(n)))
	header("Date", sent.Format(time.RFC1123Z))
	header("Message-ID", a.messageID(n))
	header("MIME-Version", "1.0")
	// Machine mail. Without this an out-of-office reply or a mailing-list
	// responder can answer a notification, and on a busy channel that is a
	// loop rather than an oddity.
	header("Auto-Submitted", "auto-generated")
	if n.RequestID != "" {
		// The delivery log's correlation key, carried into the mail so an
		// inbox and the log can be lined up without guessing from timestamps.
		header("X-Hubbub-Request-Id", n.RequestID)
	}
	if p, ok := smtpPriorityHeaders[n.Priority]; ok && p.importance != "" {
		header("Importance", p.importance)
		header("X-Priority", p.xPriority)
	}
	if len(n.Tags) > 0 {
		header("Keywords", mime.QEncoding.Encode("utf-8", strings.Join(n.Tags, ", ")))
	}
	header("Content-Type", `multipart/alternative; boundary="`+mw.Boundary()+`"`)
	msg.WriteString("\r\n")
	msg.Write(parts.Bytes())

	// No fit-to-channel truncation, unlike ntfy's 4096-byte body: the ingest
	// caps (256-byte title, 4 KB message, 128 KB html) are already an order of
	// magnitude under what submission servers accept, so there is no length at
	// which a valid notification would be refused for size.
	return msg.Bytes(), nil
}

func writeMIMEPart(mw *multipart.Writer, contentType string, body []byte) error {
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", contentType)
	// quoted-printable rather than 8bit: it keeps every line inside the
	// 998-octet limit RFC 5322 sets and survives a relay that only speaks
	// 7-bit. Those are exactly the two ways a long minified HTML line arrives
	// mangled, and both are invisible until someone opens the mail.
	h.Set("Content-Transfer-Encoding", "quoted-printable")
	pw, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	qp := quotedprintable.NewWriter(pw)
	if _, err := qp.Write(body); err != nil {
		return err
	}
	return qp.Close()
}

// subjectFor prefixes the two priorities worth seeing before the mail is
// opened. Priority is display-only by design, and a subject prefix is exactly
// that: it never changes which channels fire.
func subjectFor(n notify.Notification) string {
	switch n.Priority {
	case notify.PriorityUrgent:
		return "[URGENT] " + n.Title
	case notify.PriorityHigh:
		return "[HIGH] " + n.Title
	}
	return n.Title
}

// messageID is built from the request id, which is 16 crypto/rand bytes — so
// it is unique without a counter, and stable across retries of one message.
func (a *smtpAdapter) messageID(n notify.Notification) string {
	id := n.RequestID
	if id == "" {
		id = notify.NewRequestID()
	}
	domain := "hubbub.invalid"
	if _, host, ok := strings.Cut(a.fromAddr, "@"); ok && host != "" {
		domain = host
	}
	return "<" + id + "@" + domain + ">"
}

// plainBody is the text/plain alternative: the message as the caller wrote it,
// with the context the HTML part shows as chrome appended as a short trailer.
// Never the html field — a text part containing markup is worse than no text
// part at all.
func plainBody(n notify.Notification) string {
	var b strings.Builder
	b.WriteString(n.Message)
	if !strings.HasSuffix(n.Message, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n-- \n")
	if len(n.Tags) > 0 {
		fmt.Fprintf(&b, "tags: %s\n", strings.Join(n.Tags, ", "))
	}
	if n.Priority != "" && n.Priority != notify.PriorityDefault {
		fmt.Fprintf(&b, "priority: %s\n", n.Priority)
	}
	b.WriteString("sent via hubbub")
	if n.RequestID != "" {
		fmt.Fprintf(&b, " · %s", n.RequestID)
	}
	b.WriteString("\n")
	return b.String()
}

// htmlBody decides who owns the markup.
func (a *smtpAdapter) htmlBody(n notify.Notification) ([]byte, error) {
	body := strings.TrimSpace(n.HTML)
	if body == "" {
		// Nothing from the caller: hubbub composes the mail from the fields it
		// does have, so a plain notification still arrives looking like
		// something rather than a bare line of text.
		return renderMailShell(n, escapeToParagraphs(n.Message))
	}
	// A caller that sent a whole document owns the whole document. Wrapping it
	// would nest <html> inside a <body> and leave the result to each client's
	// error recovery, which is the kind of difference that only shows up in
	// the one mail client you don't have.
	if isHTMLDocument(body) {
		return []byte(body), nil
	}
	// A fragment gets the shell, so `"<p>done</p>"` is a complete styled mail
	// rather than an unstyled orphan.
	return renderMailShell(n, template.HTML(body))
}

func isHTMLDocument(body string) bool {
	lower := strings.ToLower(body)
	return strings.HasPrefix(lower, "<!doctype") || strings.HasPrefix(lower, "<html")
}

// escapeToParagraphs turns a plain message into markup: escape first, then add
// the only tags involved. Same order as the docs page's inlineMarkdown, and
// for the same reason — every byte that came from the caller is inert text
// before any tag exists, so the only markup in the result is markup this
// function wrote.
func escapeToParagraphs(message string) template.HTML {
	var b strings.Builder
	for _, para := range strings.Split(message, "\n\n") {
		if strings.TrimSpace(para) == "" {
			continue
		}
		b.WriteString("<p style=\"margin:0 0 14px\">")
		b.WriteString(strings.ReplaceAll(template.HTMLEscapeString(strings.TrimSpace(para)), "\n", "<br>"))
		b.WriteString("</p>")
	}
	if b.Len() == 0 {
		return template.HTML("<p style=\"margin:0 0 14px\">" + template.HTMLEscapeString(message) + "</p>")
	}
	return template.HTML(b.String())
}

type mailView struct {
	Title    string
	Body     template.HTML
	Pill     string
	PillCSS  template.CSS
	Tags     []string
	Footer   string
	Preamble string
}

// Styles are inline on every element rather than in a <style> block: mail
// clients strip, rewrite or ignore document CSS at their discretion, and the
// one thing they all honour is a style attribute.
const mailShellHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Title}}</title></head>
<body style="margin:0;padding:0;background:#f4f4f2">
<span style="display:none;font-size:0;line-height:0;max-height:0;overflow:hidden">{{.Preamble}}</span>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#f4f4f2"><tr><td align="center" style="padding:24px 12px">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:600px;background:#ffffff;border:1px solid #e3e0da;border-radius:8px">
<tr><td style="padding:26px 28px 22px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif;font-size:15px;line-height:1.55;color:#22201d">
{{if .Pill}}<div style="margin:0 0 12px"><span style="display:inline-block;padding:2px 9px;border-radius:999px;font-size:11px;font-weight:700;letter-spacing:.08em;text-transform:uppercase;{{.PillCSS}}">{{.Pill}}</span></div>{{end}}
<h1 style="margin:0 0 16px;font-size:19px;line-height:1.3;font-weight:600;color:#22201d">{{.Title}}</h1>
{{.Body}}
{{if .Tags}}<div style="margin:18px 0 0;font-size:12px;color:#6b665e">{{range $i, $t := .Tags}}{{if $i}} · {{end}}{{$t}}{{end}}</div>{{end}}
</td></tr>
<tr><td style="padding:0 28px 22px"><div style="border-top:1px solid #eceae5;padding-top:14px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif;font-size:11px;line-height:1.6;color:#8a847a">{{.Footer}}</div></td></tr>
</table>
</td></tr></table>
</body></html>`

// Parsed once, lazily, and never with template.Must: a broken edit here must
// fail this channel's send — where it lands in the delivery log as a permanent
// error naming the template — rather than panic the process and take every
// other channel down with it.
var mailShell = sync.OnceValues(func() (*template.Template, error) {
	return template.New("mail").Parse(mailShellHTML)
})

var pillCSS = map[notify.Priority]template.CSS{
	notify.PriorityUrgent: "background:#fbe9e7;color:#a3241a",
	notify.PriorityHigh:   "background:#fdf0e0;color:#8a4a06",
	notify.PriorityLow:    "background:#eef1f4;color:#5c6672",
}

func renderMailShell(n notify.Notification, body template.HTML) ([]byte, error) {
	t, err := mailShell()
	if err != nil {
		return nil, err
	}

	v := mailView{Title: n.Title, Body: body, Tags: n.Tags}
	if css, ok := pillCSS[n.Priority]; ok {
		v.Pill = strings.ToUpper(string(n.Priority))
		v.PillCSS = css
	}
	// The preview line clients show next to the subject. Without it they pull
	// whatever text comes first, which for a table-based layout is often
	// nothing at all.
	v.Preamble = notify.TruncateBytes(strings.ReplaceAll(n.Message, "\n", " "), 140, "…")
	v.Footer = "Sent by hubbub"
	if n.CallerID != "" {
		v.Footer += " · " + n.CallerID
	}
	if n.RequestID != "" {
		v.Footer += " · " + n.RequestID
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
