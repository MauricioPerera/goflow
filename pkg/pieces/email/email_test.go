package email_test

import (
	"bufio"
	"encoding/base64"
	"net"
	"strings"
	"sync"
	"testing"

	"goflow/pkg/piece"
	emailpiece "goflow/pkg/pieces/email"
)

// fakeSMTPServer is a minimal, stdlib-only SMTP server (net + bufio only) the
// email piece's tests run against — same "test real I/O against a real
// server, never a mocked interface" stance as pkg/pieces/http running against
// httptest.Server. There is no httptest equivalent for SMTP, so this lives
// here in the test file (like newMux lives in http_test.go, not in a shared
// package). It implements just enough of RFC 5321 for net/smtp.SendMail (the
// client) to complete a send: greeting, EHLO (advertising AUTH PLAIN), AUTH
// PLAIN, MAIL FROM, RCPT TO, DATA, QUIT. It accepts any credentials and never
// validates anything — its only job is to capture what the client sent so the
// tests can assert on it.
type fakeSMTPServer struct {
	mu       sync.Mutex
	listener net.Listener
	from     string
	to       []string
	message  string
	authUser string
	authPass string
}

func newFakeSMTPServer(t *testing.T) *fakeSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	s := &fakeSMTPServer{listener: ln}
	go s.acceptLoop()
	return s
}

func (s *fakeSMTPServer) addr() string { return s.listener.Addr().String() }

func (s *fakeSMTPServer) close() { s.listener.Close() }

func (s *fakeSMTPServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // listener closed -> test over
		}
		s.handle(conn) // one connection at a time; fine for tests
	}
}

// snapshot returns a consistent copy of everything the server captured, under
// the lock so the test goroutine racing the server goroutine is -race clean.
func (s *fakeSMTPServer) snapshot() (from string, to []string, message, user, pass string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.from, append([]string(nil), s.to...), s.message, s.authUser, s.authPass
}

func (s *fakeSMTPServer) handle(conn net.Conn) {
	defer conn.Close()
	conn.Write([]byte("220 test.local Simple Mail Transfer Service Ready\r\n"))
	reader := bufio.NewReader(conn)

	inData := false
	var dataLines []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")

		if inData {
			if line == "." {
				s.mu.Lock()
				s.message = strings.Join(dataLines, "\r\n")
				s.mu.Unlock()
				conn.Write([]byte("250 OK: queued\r\n"))
				inData = false
				dataLines = nil
				continue
			}
			// RFC 5321 dot-unstuffing: a body line starting with ".." was
			// dot-stuffed by the client; restore the single leading dot.
			if strings.HasPrefix(line, "..") {
				line = line[1:]
			}
			dataLines = append(dataLines, line)
			continue
		}

		verb := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(verb, "EHLO"):
			conn.Write([]byte("250-test.local Hello\r\n250 AUTH PLAIN\r\n"))
		case strings.HasPrefix(verb, "HELO"):
			conn.Write([]byte("250 test.local Hello\r\n"))
		case strings.HasPrefix(verb, "AUTH PLAIN"):
			// "AUTH PLAIN <base64>"; base64 decodes to "\x00user\x00pass".
			parts := strings.SplitN(line, " ", 3)
			if len(parts) == 3 {
				if dec, err := base64.StdEncoding.DecodeString(parts[2]); err == nil {
					fields := strings.Split(string(dec), "\x00")
					if len(fields) == 3 {
						s.mu.Lock()
						s.authUser = fields[1]
						s.authPass = fields[2]
						s.mu.Unlock()
					}
				}
			}
			conn.Write([]byte("235 Authentication successful\r\n"))
		case strings.HasPrefix(verb, "MAIL FROM"):
			s.mu.Lock()
			s.from = parseAddrArg(line, "FROM:")
			s.mu.Unlock()
			conn.Write([]byte("250 OK\r\n"))
		case strings.HasPrefix(verb, "RCPT TO"):
			s.mu.Lock()
			s.to = append(s.to, parseAddrArg(line, "TO:"))
			s.mu.Unlock()
			conn.Write([]byte("250 OK\r\n"))
		case verb == "DATA":
			conn.Write([]byte("354 Start mail input; end with <CRLF>.<CRLF>\r\n"))
			inData = true
			dataLines = nil
		case strings.HasPrefix(verb, "QUIT"):
			conn.Write([]byte("221 test.local Bye\r\n"))
			return
		case strings.HasPrefix(verb, "RSET"), strings.HasPrefix(verb, "NOOP"):
			conn.Write([]byte("250 OK\r\n"))
		default:
			conn.Write([]byte("500 Command unrecognized\r\n"))
		}
	}
}

// parseAddrArg pulls the address out of "MAIL FROM:<a@b>" / "RCPT TO:<a@b>",
// taking whatever is between < and > when angle brackets are present, or the
// raw remainder after the key otherwise.
func parseAddrArg(line, key string) string {
	idx := strings.Index(strings.ToUpper(line), key)
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(line[idx+len(key):])
	if strings.HasPrefix(rest, "<") {
		if end := strings.Index(rest, ">"); end >= 0 {
			return rest[1:end]
		}
	}
	return rest
}

func TestEmail_SendSimpleNoAuth(t *testing.T) {
	srv := newFakeSMTPServer(t)
	defer srv.close()

	p := emailpiece.New()
	act := p.Actions["send"]

	out, err := act.Run(piece.ActionContext{Input: map[string]any{
		"smtpAddr": srv.addr(),
		"from":     "from@test.local",
		"to":       []any{"to@test.local"},
		"subject":  "hello",
		"body":     "world",
	}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	m := out.(map[string]any)
	if m["sent"] != true || m["to"] != 1 {
		t.Fatalf("out = %+v, want sent=true to=1", m)
	}

	from, to, message, user, pass := srv.snapshot()
	if from != "from@test.local" {
		t.Fatalf("server saw from = %q, want from@test.local", from)
	}
	if len(to) != 1 || to[0] != "to@test.local" {
		t.Fatalf("server saw to = %v, want [to@test.local]", to)
	}
	if user != "" || pass != "" {
		t.Fatalf("server saw auth user=%q pass=%q, want no auth at all", user, pass)
	}
	if !strings.Contains(message, "Subject: hello") {
		t.Fatalf("message missing Subject: %q", message)
	}
	if !strings.Contains(message, "From: from@test.local") {
		t.Fatalf("message missing From: %q", message)
	}
	if !strings.Contains(message, "To: to@test.local") {
		t.Fatalf("message missing To: %q", message)
	}
	if !strings.Contains(message, "Content-Type: text/plain; charset=utf-8") {
		t.Fatalf("message missing plain Content-Type: %q", message)
	}
	if !strings.Contains(message, "MIME-Version: 1.0") {
		t.Fatalf("message missing MIME-Version: %q", message)
	}
	if !strings.HasSuffix(message, "world") {
		t.Fatalf("message body not at end: %q", message)
	}
}

func TestEmail_SendMultipleRecipients(t *testing.T) {
	srv := newFakeSMTPServer(t)
	defer srv.close()

	p := emailpiece.New()
	act := p.Actions["send"]

	out, err := act.Run(piece.ActionContext{Input: map[string]any{
		"smtpAddr": srv.addr(),
		"from":     "from@test.local",
		"to":       []any{"a@test.local", "b@test.local", "c@test.local"},
		"subject":  "multi",
		"body":     "hi",
	}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	m := out.(map[string]any)
	if m["sent"] != true || m["to"] != 3 {
		t.Fatalf("out = %+v, want sent=true to=3", m)
	}

	_, to, _, _, _ := srv.snapshot()
	if len(to) != 3 {
		t.Fatalf("server received %d RCPT TO, want 3 (%v)", len(to), to)
	}
}

func TestEmail_SendWithAuth(t *testing.T) {
	srv := newFakeSMTPServer(t)
	defer srv.close()

	p := emailpiece.New()
	act := p.Actions["send"]

	_, err := act.Run(piece.ActionContext{
		Input: map[string]any{
			"smtpAddr": srv.addr(),
			"from":     "from@test.local",
			"to":       []any{"to@test.local"},
			"subject":  "auth",
			"body":     "x",
		},
		Auth: "user:pass",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	_, _, _, user, pass := srv.snapshot()
	if user != "user" || pass != "pass" {
		t.Fatalf("server saw AUTH user=%q pass=%q, want user/pass (PLAIN-decoded)", user, pass)
	}
}

func TestEmail_AuthWithColonInPassword(t *testing.T) {
	srv := newFakeSMTPServer(t)
	defer srv.close()

	p := emailpiece.New()
	act := p.Actions["send"]

	_, err := act.Run(piece.ActionContext{
		Input: map[string]any{
			"smtpAddr": srv.addr(),
			"from":     "from@test.local",
			"to":       []any{"to@test.local"},
			"subject":  "auth",
			"body":     "x",
		},
		Auth: "user:pa:ss", // split on the FIRST colon only
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	_, _, _, user, pass := srv.snapshot()
	if user != "user" || pass != "pa:ss" {
		t.Fatalf("server saw AUTH user=%q pass=%q, want user / pa:ss (split on first colon)", user, pass)
	}
}

func TestEmail_HTMLContentType(t *testing.T) {
	srv := newFakeSMTPServer(t)
	defer srv.close()

	p := emailpiece.New()
	act := p.Actions["send"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{
		"smtpAddr": srv.addr(),
		"from":     "from@test.local",
		"to":       []any{"to@test.local"},
		"subject":  "html",
		"body":     "<b>hi</b>",
		"html":     true,
	}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	_, _, message, _, _ := srv.snapshot()
	if !strings.Contains(message, "Content-Type: text/html; charset=utf-8") {
		t.Fatalf("html:true message missing html Content-Type: %q", message)
	}
	if strings.Contains(message, "text/plain") {
		t.Fatalf("html:true message should not carry text/plain: %q", message)
	}
}

func TestEmail_PlainContentTypeWhenHTMLFalse(t *testing.T) {
	srv := newFakeSMTPServer(t)
	defer srv.close()

	p := emailpiece.New()
	act := p.Actions["send"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{
		"smtpAddr": srv.addr(),
		"from":     "from@test.local",
		"to":       []any{"to@test.local"},
		"subject":  "plain",
		"body":     "hi",
		"html":     false,
	}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	_, _, message, _, _ := srv.snapshot()
	if !strings.Contains(message, "Content-Type: text/plain; charset=utf-8") {
		t.Fatalf("html:false message missing plain Content-Type: %q", message)
	}
}

func TestEmail_MissingToFailsWithoutConnecting(t *testing.T) {
	srv := newFakeSMTPServer(t)
	defer srv.close()

	p := emailpiece.New()
	act := p.Actions["send"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{
		"smtpAddr": srv.addr(),
		"from":     "from@test.local",
		// "to" intentionally omitted
		"subject": "x",
		"body":    "x",
	}})
	if err == nil {
		t.Fatal("Run() error = nil, want a missing-to error")
	}
	if !strings.Contains(err.Error(), "missing required input: to") {
		t.Fatalf("err = %q, want it to name the missing to input", err.Error())
	}
	// A missing required input must fail BEFORE any network call: the server
	// should have seen no MAIL FROM at all.
	if from, _, _, _, _ := srv.snapshot(); from != "" {
		t.Fatalf("server saw MAIL FROM = %q, want no connection to have been made", from)
	}
}

func TestEmail_MissingSubjectFailsWithoutConnecting(t *testing.T) {
	srv := newFakeSMTPServer(t)
	defer srv.close()

	p := emailpiece.New()
	act := p.Actions["send"]

	_, err := act.Run(piece.ActionContext{Input: map[string]any{
		"smtpAddr": srv.addr(),
		"from":     "from@test.local",
		"to":       []any{"to@test.local"},
		// "subject" intentionally omitted
		"body": "x",
	}})
	if err == nil {
		t.Fatal("Run() error = nil, want a missing-subject error")
	}
	if !strings.Contains(err.Error(), "missing required input: subject") {
		t.Fatalf("err = %q, want it to name the missing subject input", err.Error())
	}
	if from, _, _, _, _ := srv.snapshot(); from != "" {
		t.Fatalf("server saw MAIL FROM = %q, want no connection to have been made", from)
	}
}

func TestEmail_ClosedPortFailsClearly(t *testing.T) {
	// Grab a free port, then close it so nothing is listening there — a
	// deterministic "connection refused" target rather than guessing a port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	p := emailpiece.New()
	act := p.Actions["send"]

	_, err = act.Run(piece.ActionContext{Input: map[string]any{
		"smtpAddr": addr,
		"from":     "from@test.local",
		"to":       []any{"to@test.local"},
		"subject":  "x",
		"body":     "x",
	}})
	if err == nil {
		t.Fatal("Run() error = nil, want a connection failure to a closed port")
	}
	if !strings.Contains(err.Error(), "email: sending:") {
		t.Fatalf("err = %q, want it wrapped with email: sending:", err.Error())
	}
}
