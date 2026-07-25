package adapter

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ryanlewis/hubbub/internal/notify"
)

// The tests below talk real SMTP to an in-process server rather than stubbing
// the adapter's seams. The bugs worth catching here are protocol-shaped —
// credentials sent before the connection is encrypted, a reply code classified
// as the wrong retry kind, a header the client and server disagree about — and
// none of them are visible from inside the adapter.

// --- a fake submission server ------------------------------------------------

type smtpScript struct {
	// replies overrides the answer to a command verb, keyed by verb (EHLO,
	// AUTH, MAIL, RCPT, DATA) plus the pseudo-verb BODY for the reply that
	// lands after the message's terminating dot.
	replies map[string]string
	noTLS   bool // don't advertise STARTTLS, to test the downgrade refusal
	hang    bool // accept and never speak, to test the attempt timeout
}

type fakeSMTP struct {
	addr    string
	tlsConf *tls.Config
	script  smtpScript

	done  chan struct{}
	conns sync.WaitGroup

	mu          sync.Mutex
	helo        string
	authPayload string
	authOverTLS bool
	mailFrom    string
	rcptTo      []string
	data        string
}

func newFakeSMTP(t *testing.T, script smtpScript, serverTLS *tls.Config) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{addr: ln.Addr().String(), tlsConf: serverTLS, script: script, done: make(chan struct{})}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.conns.Go(func() { s.serve(conn) })
		}
	}()

	t.Cleanup(func() {
		close(s.done)
		ln.Close()
		s.conns.Wait()
	})
	return s
}

func (s *fakeSMTP) host(t *testing.T) (string, int) {
	t.Helper()
	h, p, err := net.SplitHostPort(s.addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(p)
	return h, port
}

// settled waits for the connection handler to finish, so a test reads what the
// server captured rather than racing it. Send closing the socket on its way out
// is what ends the handler's read.
func (s *fakeSMTP) settled() { s.conns.Wait() }

func (s *fakeSMTP) body(t *testing.T) string {
	t.Helper()
	s.settled()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == "" {
		t.Fatal("server received no message body")
	}
	return s.data
}

func (s *fakeSMTP) serve(conn net.Conn) {
	defer func() { conn.Close() }()
	if s.script.hang {
		<-s.done // accepted, never answered: the client must time itself out
		return
	}

	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)
	say := func(format string, a ...any) {
		fmt.Fprintf(bw, format+"\r\n", a...)
		bw.Flush()
	}
	upgraded := false

	say("220 fake.test ESMTP hubbub-test")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		verb, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		verb = strings.ToUpper(verb)

		// DATA is scripted below, because its override has to replace the 354
		// rather than the final reply.
		if reply, ok := s.script.replies[verb]; ok && verb != "DATA" {
			say("%s", reply)
			continue
		}

		switch verb {
		case "EHLO", "HELO":
			s.mu.Lock()
			s.helo = rest
			s.mu.Unlock()
			say("250-fake.test")
			// Advertising STARTTLS again after the upgrade would be a protocol
			// error, and the client would be right to be confused by it.
			if s.tlsConf != nil && !upgraded && !s.script.noTLS {
				say("250-STARTTLS")
			}
			say("250-AUTH PLAIN LOGIN")
			say("250 8BITMIME")

		case "STARTTLS":
			say("220 2.0.0 ready to start TLS")
			tconn := tls.Server(conn, s.tlsConf)
			if err := tconn.Handshake(); err != nil {
				return
			}
			conn = tconn
			br, bw = bufio.NewReader(conn), bufio.NewWriter(conn)
			upgraded = true

		case "AUTH":
			s.mu.Lock()
			_, s.authPayload, _ = strings.Cut(rest, " ")
			s.authOverTLS = upgraded
			s.mu.Unlock()
			say("235 2.7.0 authentication succeeded")

		case "MAIL":
			s.mu.Lock()
			s.mailFrom = rest
			s.mu.Unlock()
			say("250 2.1.0 sender ok")

		case "RCPT":
			s.mu.Lock()
			s.rcptTo = append(s.rcptTo, rest)
			s.mu.Unlock()
			say("250 2.1.5 recipient ok")

		case "DATA":
			if reply, ok := s.script.replies["DATA"]; ok {
				say("%s", reply)
				continue
			}
			say("354 end data with <CRLF>.<CRLF>")
			var body strings.Builder
			for {
				l, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" || l == ".\n" {
					break
				}
				// Undo dot-stuffing, so the test sees the message as written.
				if strings.HasPrefix(l, "..") {
					l = l[1:]
				}
				body.WriteString(l)
			}
			s.mu.Lock()
			s.data = body.String()
			s.mu.Unlock()
			if reply, ok := s.script.replies["BODY"]; ok {
				say("%s", reply)
				continue
			}
			say("250 2.0.0 queued")

		case "QUIT":
			say("221 2.0.0 bye")
			return

		case "RSET":
			say("250 2.0.0 ok")

		default:
			say("502 5.5.2 unrecognised command")
		}
	}
}

// selfSignedTLS mints a throwaway CA-of-one for 127.0.0.1, so the STARTTLS
// tests exercise real certificate verification. The alternative — an
// "insecure_skip_verify" config key — would put a footgun in the shipped
// adapter purely to make its tests easier.
func selfSignedTLS(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake.test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"fake.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}}},
		&tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
}

// --- adapter construction ----------------------------------------------------

// smtpTo builds an adapter aimed at a fake server over a bare connection,
// which keeps the message-shape tests free of certificate machinery. It cannot
// carry a password — the factory refuses that pairing — so anything touching
// AUTH uses smtpTLSTo instead.
func smtpTo(t *testing.T, s *fakeSMTP, extra string) *smtpAdapter {
	t.Helper()
	return buildSMTP(t, s, "none", extra, nil)
}

// smtpTLSTo is the realistic configuration: submission over STARTTLS with
// credentials, verified against the throwaway CA the fake server presents.
func smtpTLSTo(t *testing.T, s *fakeSMTP, clientTLS *tls.Config, extra string) *smtpAdapter {
	t.Helper()
	a := buildSMTP(t, s, "starttls", "username = \"hub@example.test\"\npassword = \"app-specific-pw\"\n"+extra, clientTLS)
	return a
}

func buildSMTP(t *testing.T, s *fakeSMTP, mode, extra string, clientTLS *tls.Config) *smtpAdapter {
	t.Helper()
	host, port := s.host(t)
	cfg := fmt.Sprintf("host = %q\nport = %d\nfrom = \"hub@example.test\"\nfrom_name = \"hubbub\"\nto = [\"ops@example.test\"]\ntls = %q\n%s",
		host, port, mode, extra)
	a, err := newSMTP("email", cfgFrom(cfg))
	if err != nil {
		t.Fatal(err)
	}
	a.tlsConfig = clientTLS
	return a
}

func mailNote() notify.Notification {
	return notify.Notification{
		Title:     "Backup failed",
		Message:   "nightly borg run exited 1\nrepo: /srv/borg",
		Priority:  notify.PriorityHigh,
		Tags:      []string{"borg", "cron"},
		CallerID:  "cron-key",
		RequestID: "r_deadbeef",
		CreatedAt: time.Date(2026, 7, 26, 14, 2, 0, 0, time.UTC),
	}
}

// parseMail reads the message back the way a mail client would: real header
// parsing, real MIME walking, real transfer decoding. Asserting on the raw
// bytes instead would pass on a message no client could open.
func parseMail(t *testing.T, raw string) (mail.Header, string, string) {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("message does not parse: %v", err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("content-type does not parse: %v", err)
	}
	if mediaType != "multipart/alternative" {
		t.Fatalf("content-type = %q, want multipart/alternative", mediaType)
	}

	var plain, html string
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading parts: %v", err)
		}
		var body io.Reader = part
		if strings.EqualFold(part.Header.Get("Content-Transfer-Encoding"), "quoted-printable") {
			body = quotedprintable.NewReader(part)
		}
		decoded, err := io.ReadAll(body)
		if err != nil {
			t.Fatalf("decoding part: %v", err)
		}
		switch ct := part.Header.Get("Content-Type"); {
		case strings.HasPrefix(ct, "text/plain"):
			plain = string(decoded)
		case strings.HasPrefix(ct, "text/html"):
			html = string(decoded)
		default:
			t.Errorf("unexpected part content-type %q", ct)
		}
	}
	if plain == "" || html == "" {
		t.Fatalf("want both a plain and an html part, got plain=%d bytes html=%d bytes", len(plain), len(html))
	}
	return msg.Header, plain, html
}

// decodeHeader undoes RFC 2047 encoded words. mail.Header.Get deliberately
// does not, so reading a Subject the way a client displays it takes this.
func decodeHeader(t *testing.T, v string) string {
	t.Helper()
	decoded, err := new(mime.WordDecoder).DecodeHeader(v)
	if err != nil {
		t.Fatalf("header %q does not decode: %v", v, err)
	}
	return decoded
}

// crlf normalises line endings for comparison. MIME bodies are canonically
// CRLF-terminated, and quoted-printable rewrites bare newlines on the way out,
// so a byte-for-byte match against a Go string literal never holds.
func crlf(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

// --- tests -------------------------------------------------------------------

// TestSMTPSendsBothAlternatives is the baseline: a notification with no html
// field still arrives as a rendered mail, and the plain part is intact for a
// client that won't show the other one.
func TestSMTPSendsBothAlternatives(t *testing.T) {
	srv := newFakeSMTP(t, smtpScript{}, nil)
	if err := smtpTo(t, srv, "").Send(context.Background(), mailNote()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	hdr, plain, html := parseMail(t, srv.body(t))

	if !strings.Contains(plain, "nightly borg run exited 1") {
		t.Errorf("plain part lost the message:\n%s", plain)
	}
	if strings.Contains(plain, "<p") || strings.Contains(plain, "<div") {
		t.Errorf("plain part carries markup:\n%s", plain)
	}
	if !strings.Contains(html, "Backup failed") || !strings.Contains(html, "nightly borg run exited 1") {
		t.Errorf("html part lost the content:\n%s", html)
	}
	// A newline inside a paragraph has to become a <br>, or a multi-line log
	// excerpt renders as one run-on line.
	if !strings.Contains(html, "repo: /srv/borg") || !strings.Contains(html, "<br>") {
		t.Errorf("html part did not keep the message's line breaks:\n%s", html)
	}
	// html/template emits ZgotmplZ when it refuses to interpolate a value it
	// can't prove safe — the priority pill's CSS is the one place that risk
	// lives, and the failure is silent otherwise.
	if strings.Contains(html, "ZgotmplZ") {
		t.Errorf("html/template refused to interpolate a value:\n%s", html)
	}

	if got := hdr.Get("From"); got != `"hubbub" <hub@example.test>` {
		t.Errorf("From = %q", got)
	}
	if got := hdr.Get("Auto-Submitted"); got != "auto-generated" {
		t.Errorf("Auto-Submitted = %q, want auto-generated (machine mail must not draw autoresponders)", got)
	}
	if got := hdr.Get("X-Hubbub-Request-Id"); got != "r_deadbeef" {
		t.Errorf("X-Hubbub-Request-Id = %q", got)
	}
	if got := hdr.Get("Importance"); got != "High" {
		t.Errorf("Importance = %q, want High for a high-priority notification", got)
	}
}

// TestSMTPCallerHTMLIsUsedAsWritten covers the whole point of the field: the
// producer owns the body. A fragment gets hubbub's shell so a one-liner still
// looks like an email; a whole document is left alone, because wrapping it
// would nest <html> inside <body>.
func TestSMTPCallerHTMLIsUsedAsWritten(t *testing.T) {
	t.Run("fragment is wrapped", func(t *testing.T) {
		srv := newFakeSMTP(t, smtpScript{}, nil)
		n := mailNote()
		n.HTML = `<table id="report"><tr><td>borg</td><td>1</td></tr></table>`

		if err := smtpTo(t, srv, "").Send(context.Background(), n); err != nil {
			t.Fatalf("Send: %v", err)
		}
		_, plain, html := parseMail(t, srv.body(t))

		if !strings.Contains(html, n.HTML) {
			t.Errorf("caller markup was altered:\n%s", html)
		}
		if !strings.Contains(html, "<!doctype html>") || !strings.Contains(html, "Sent by hubbub") {
			t.Errorf("fragment was not wrapped in the shell:\n%s", html)
		}
		// The plain alternative must stay the message. A text part full of
		// tags is worse than no text part at all.
		if strings.Contains(plain, "<table") {
			t.Errorf("html leaked into the plain part:\n%s", plain)
		}
		if !strings.Contains(plain, "nightly borg run exited 1") {
			t.Errorf("plain part is not the message:\n%s", plain)
		}
	})

	t.Run("whole document is left alone", func(t *testing.T) {
		srv := newFakeSMTP(t, smtpScript{}, nil)
		n := mailNote()
		n.HTML = "<!DOCTYPE html>\n<html><body><h1>mine</h1></body></html>"

		if err := smtpTo(t, srv, "").Send(context.Background(), n); err != nil {
			t.Fatalf("Send: %v", err)
		}
		_, _, html := parseMail(t, srv.body(t))

		if got := strings.TrimSpace(crlf(html)); got != n.HTML {
			t.Errorf("document was not used verbatim:\n%s", got)
		}
		if strings.Contains(html, "Sent by hubbub") {
			t.Error("the shell was applied on top of a caller's complete document")
		}
	})
}

// TestSMTPSubjectIsPrefixedAndEncoded pins the two things a subject line has to
// survive: a priority prefix, and any byte a caller can put in a title.
func TestSMTPSubjectIsPrefixedAndEncoded(t *testing.T) {
	for _, tc := range []struct {
		name     string
		title    string
		priority notify.Priority
		want     string
	}{
		{"urgent is prefixed", "Disk full", notify.PriorityUrgent, "[URGENT] Disk full"},
		{"high is prefixed", "Disk full", notify.PriorityHigh, "[HIGH] Disk full"},
		{"default is not", "Disk full", notify.PriorityDefault, "Disk full"},
		{"low is not", "Disk full", notify.PriorityLow, "Disk full"},
		{"unicode survives", "Sauvegarde échouée ⚠", notify.PriorityDefault, "Sauvegarde échouée ⚠"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeSMTP(t, smtpScript{}, nil)
			n := mailNote()
			n.Title, n.Priority = tc.title, tc.priority

			if err := smtpTo(t, srv, "").Send(context.Background(), n); err != nil {
				t.Fatalf("Send: %v", err)
			}
			// mail.Header.Get decodes RFC 2047 words, so this asserts the
			// subject a client shows, not the bytes on the wire.
			hdr, _, _ := parseMail(t, srv.body(t))
			if got := decodeHeader(t, hdr.Get("Subject")); got != tc.want {
				t.Errorf("Subject = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSMTPTitleCannotInjectHeaders. A title is the one caller-controlled value
// that lands in a header, so a CR/LF in it must not be able to start a header
// of the caller's choosing. Ingest strips control characters already — this is
// the adapter holding the line on its own.
func TestSMTPTitleCannotInjectHeaders(t *testing.T) {
	srv := newFakeSMTP(t, smtpScript{}, nil)
	n := mailNote()
	n.Title = "ok\r\nBcc: attacker@evil.test\r\nX-Injected: yes"
	n.Priority = notify.PriorityDefault

	if err := smtpTo(t, srv, "").Send(context.Background(), n); err != nil {
		t.Fatalf("Send: %v", err)
	}

	raw := srv.body(t)
	hdr, _, _ := parseMail(t, raw)
	if got := hdr.Get("Bcc"); got != "" {
		t.Errorf("title injected a Bcc header: %q", got)
	}
	if got := hdr.Get("X-Injected"); got != "" {
		t.Errorf("title injected an X-Injected header: %q", got)
	}
	// Only one recipient may have been asked for on the envelope, whatever the
	// headers say.
	srv.mu.Lock()
	rcpts := append([]string(nil), srv.rcptTo...)
	srv.mu.Unlock()
	if len(rcpts) != 1 || !strings.Contains(rcpts[0], "ops@example.test") {
		t.Errorf("envelope recipients = %v, want just ops@example.test", rcpts)
	}
}

// TestSMTPClassifiesReplyCodes is the contract the outbox worker acts on: a
// 4xx has to come back retryable and a 5xx permanent, or a transient refusal
// silently drops a notification (or a rejected one is retried until TTL).
func TestSMTPClassifiesReplyCodes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replies map[string]string
		want    Kind
	}{
		{"5xx recipient is permanent", map[string]string{"RCPT": "550 5.1.1 no such user"}, KindPermanent},
		{"4xx recipient is retryable", map[string]string{"RCPT": "450 4.2.0 mailbox busy"}, KindRetryable},
		{"auth failure is permanent", map[string]string{"AUTH": "535 5.7.8 bad credentials"}, KindPermanent},
		{"4xx on the body is retryable", map[string]string{"BODY": "451 4.3.0 write error"}, KindRetryable},
		{"5xx on the body is permanent", map[string]string{"BODY": "554 5.6.0 message rejected"}, KindPermanent},
		{"4xx data is retryable", map[string]string{"DATA": "421 4.3.2 shutting down"}, KindRetryable},
		{"a send limit is rate limited", map[string]string{"MAIL": "452 4.5.3 too many messages this hour"}, KindRateLimited},
		{"a quota notice is rate limited", map[string]string{"RCPT": "450 4.7.1 daily sending limit exceeded"}, KindRateLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Driven over STARTTLS with credentials, so the AUTH step is a
			// real one and the classification table covers the whole
			// conversation rather than the half of it a bare connection has.
			serverTLS, clientTLS := selfSignedTLS(t)
			srv := newFakeSMTP(t, smtpScript{replies: tc.replies}, serverTLS)
			err := smtpTLSTo(t, srv, clientTLS, "rate_limit_cooldown = \"20m\"\n").
				Send(context.Background(), mailNote())

			se, ok := err.(*SendError)
			if !ok {
				t.Fatalf("err = %v (%T), want *SendError", err, err)
			}
			if se.Kind != tc.want {
				t.Errorf("kind = %v, want %v (reason %q)", se.Kind, tc.want, se.Reason)
			}
			if tc.want == KindRateLimited {
				if d := time.Until(se.NotBefore); d < 19*time.Minute || d > 21*time.Minute {
					t.Errorf("NotBefore in %v, want the configured ~20m cooldown", d)
				}
			}
		})
	}
}

// TestSMTPSendsCredentialsOnlyOverTLS. PlainAuth makes an exception for
// localhost, which is exactly where these tests run — so this drives a real
// STARTTLS upgrade and checks the ordering directly rather than trusting it.
func TestSMTPSendsCredentialsOnlyOverTLS(t *testing.T) {
	serverTLS, clientTLS := selfSignedTLS(t)
	srv := newFakeSMTP(t, smtpScript{}, serverTLS)

	a := smtpTLSTo(t, srv, clientTLS, "")
	if err := a.Send(context.Background(), mailNote()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	srv.settled()

	srv.mu.Lock()
	payload, overTLS := srv.authPayload, srv.authOverTLS
	srv.mu.Unlock()

	if !overTLS {
		t.Error("credentials were sent before the connection was encrypted")
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("AUTH payload is not base64: %q", payload)
	}
	// RFC 4616: an authorisation identity, the authentication identity and the
	// password, NUL-separated.
	if want := "\x00hub@example.test\x00app-specific-pw"; string(raw) != want {
		t.Errorf("AUTH payload = %q, want %q", raw, want)
	}
}

// TestSMTPRefusesToDowngrade: a submission server that has stopped offering
// STARTTLS is either broken or being stripped in transit. Sending anyway would
// put the app password on the wire in both cases, so this must fail — and fail
// retryably, since it is usually the network having a bad day.
func TestSMTPRefusesToDowngrade(t *testing.T) {
	serverTLS, clientTLS := selfSignedTLS(t)
	srv := newFakeSMTP(t, smtpScript{noTLS: true}, serverTLS)

	err := smtpTLSTo(t, srv, clientTLS, "").Send(context.Background(), mailNote())
	se, ok := err.(*SendError)
	if !ok {
		t.Fatalf("err = %v (%T), want *SendError", err, err)
	}
	if se.Kind != KindRetryable {
		t.Errorf("kind = %v, want retryable", se.Kind)
	}
	srv.settled()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.authPayload != "" {
		t.Error("credentials were sent to a server that never offered STARTTLS")
	}
	if srv.data != "" {
		t.Error("the message was delivered over an unencrypted connection")
	}
}

// TestSMTPHonoursTheAttemptTimeout. net/smtp has no context support at all, so
// the per-attempt timeout the worker sets is enforced by this adapter or by
// nothing — and "by nothing" means one unresponsive server wedges a channel's
// only delivery goroutine indefinitely.
func TestSMTPHonoursTheAttemptTimeout(t *testing.T) {
	srv := newFakeSMTP(t, smtpScript{hang: true}, nil)
	a := smtpTo(t, srv, "")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := a.Send(ctx, mailNote())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Send succeeded against a server that never replied")
	}
	if se, ok := err.(*SendError); !ok || se.Kind != KindRetryable {
		t.Errorf("err = %v, want a retryable SendError", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Send took %v to notice a 150ms deadline", elapsed)
	}
}

// TestSMTPMessageIDIsStableAcrossRetries. Delivery is at-least-once, so a
// duplicate submission is expected rather than exceptional; a stable id is what
// lets the receiving client file it as the message it already has instead of
// showing it twice.
func TestSMTPMessageIDIsStableAcrossRetries(t *testing.T) {
	srv := newFakeSMTP(t, smtpScript{}, nil)
	a := smtpTo(t, srv, "")
	n := mailNote()

	first, err := a.compose(n)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.compose(n)
	if err != nil {
		t.Fatal(err)
	}

	idOf := func(raw []byte) string {
		t.Helper()
		msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		return msg.Header.Get("Message-ID")
	}
	if a, b := idOf(first), idOf(second); a != b {
		t.Errorf("Message-ID changed between attempts: %q then %q", a, b)
	}
	if got := idOf(first); !strings.Contains(got, "r_deadbeef@example.test") {
		t.Errorf("Message-ID = %q, want it built from the request id and the from domain", got)
	}
}

func TestSMTPRejectsBadConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  string
	}{
		{"no host", "from = \"a@b.test\"\nto = [\"c@d.test\"]\n"},
		{"no from", "host = \"smtp.test\"\nto = [\"c@d.test\"]\n"},
		{"no recipients", "host = \"smtp.test\"\nfrom = \"a@b.test\"\n"},
		{"empty recipient list", "host = \"smtp.test\"\nfrom = \"a@b.test\"\nto = []\n"},
		{"unparseable from", "host = \"smtp.test\"\nfrom = \"not an address\"\nto = [\"c@d.test\"]\n"},
		{"unparseable recipient", "host = \"smtp.test\"\nfrom = \"a@b.test\"\nto = [\"@@\"]\n"},
		{"unknown tls mode", "host = \"smtp.test\"\nfrom = \"a@b.test\"\nto = [\"c@d.test\"]\ntls = \"maybe\"\n"},
		{"port out of range", "host = \"smtp.test\"\nfrom = \"a@b.test\"\nto = [\"c@d.test\"]\nport = 70000\n"},
		{"bad cooldown", "host = \"smtp.test\"\nfrom = \"a@b.test\"\nto = [\"c@d.test\"]\nrate_limit_cooldown = \"soon\"\n"},
		{"negative cooldown", "host = \"smtp.test\"\nfrom = \"a@b.test\"\nto = [\"c@d.test\"]\nrate_limit_cooldown = \"-5m\"\n"},
		// The one that would otherwise look configured and fail every send:
		// PlainAuth refuses to transmit over a bare connection.
		{"password without tls", "host = \"smtp.test\"\nfrom = \"a@b.test\"\nto = [\"c@d.test\"]\ntls = \"none\"\npassword = \"pw\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New("smtp", "email", cfgFrom(tc.cfg)); err == nil {
				t.Error("config was accepted, want a validation error")
			}
		})
	}
}

// TestSMTPDefaultsFollowThePort: 465 is the implicit-TLS submission port and
// 587 the STARTTLS one, so an operator who set only the port has already said
// which handshake they meant.
func TestSMTPDefaultsFollowThePort(t *testing.T) {
	base := "host = \"smtp.mail.me.com\"\nfrom = \"a@b.test\"\nto = [\"c@d.test\"]\n"
	for _, tc := range []struct {
		name     string
		cfg      string
		wantMode string
		wantAddr string
	}{
		{"no port means submission over starttls", base, tlsSTARTTLS, "smtp.mail.me.com:587"},
		{"465 means implicit tls", base + "port = 465\n", tlsImplicit, "smtp.mail.me.com:465"},
		{"587 means starttls", base + "port = 587\n", tlsSTARTTLS, "smtp.mail.me.com:587"},
		{"an explicit mode wins", base + "tls = \"implicit\"\n", tlsImplicit, "smtp.mail.me.com:465"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := newSMTP("email", cfgFrom(tc.cfg))
			if err != nil {
				t.Fatal(err)
			}
			if a.mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", a.mode, tc.wantMode)
			}
			if a.addr != tc.wantAddr {
				t.Errorf("addr = %q, want %q", a.addr, tc.wantAddr)
			}
		})
	}
}

// TestSMTPSendsToEveryRecipient: `to` is a list, and a channel configured with
// two addresses that only ever reaches the first is a silent half-failure.
func TestSMTPSendsToEveryRecipient(t *testing.T) {
	srv := newFakeSMTP(t, smtpScript{}, nil)
	host, port := srv.host(t)
	a, err := newSMTP("email", cfgFrom(fmt.Sprintf(
		"host = %q\nport = %d\nfrom = \"hub@example.test\"\nto = [\"one@example.test\", \"Two <two@example.test>\"]\ntls = \"none\"\n",
		host, port)))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Send(context.Background(), mailNote()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	srv.settled()
	srv.mu.Lock()
	rcpts := strings.Join(srv.rcptTo, " ")
	srv.mu.Unlock()
	for _, want := range []string{"one@example.test", "two@example.test"} {
		if !strings.Contains(rcpts, want) {
			t.Errorf("envelope recipients %q missing %q", rcpts, want)
		}
	}

	hdr, _, _ := parseMail(t, srv.body(t))
	// The display name belongs in the header and must not reach the envelope.
	if got := hdr.Get("To"); !strings.Contains(got, `"Two" <two@example.test>`) {
		t.Errorf("To header = %q, want the display name preserved", got)
	}
}
