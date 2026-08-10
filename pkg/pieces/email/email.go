// Package email provides goflow's "Email" catalog piece: send a real email
// via Go's net/smtp (SendMail, which speaks the full SMTP protocol itself),
// authenticating with smtp.PlainAuth when ctx.Auth is a "user:password"
// string and sending unauthenticated otherwise (an open relay / no-auth
// server is a valid case, not a failure). The message is assembled by hand
// to RFC 5322; net/smtp handles the wire. Same "real I/O tested against a
// real server" stance as pkg/pieces/http — see email_test.go for the
// stdlib-only fake SMTP server the tests run against.
package email

import (
	"fmt"
	"net"
	"net/smtp"
	"strings"

	"goflow/pkg/piece"
)

const PieceName = "email"

func New() piece.Piece {
	p := piece.Piece{
		Name: PieceName, DisplayName: "Email",
		Actions: map[string]piece.Action{
			"send": sendAction(),
		},
	}
	piece.MustValidate(p)
	return p
}

func sendAction() piece.Action {
	return piece.Action{
		Name: "send", DisplayName: "Send Email",
		Run: func(ctx piece.ActionContext) (any, error) {
			smtpAddr, ok := ctx.Input["smtpAddr"].(string)
			if !ok || smtpAddr == "" {
				return nil, fmt.Errorf("missing required input: smtpAddr (string)")
			}
			from, ok := ctx.Input["from"].(string)
			if !ok || from == "" {
				return nil, fmt.Errorf("missing required input: from (string)")
			}
			toRaw, ok := ctx.Input["to"].([]any)
			if !ok || len(toRaw) == 0 {
				return nil, fmt.Errorf("missing required input: to ([]string)")
			}
			recipients := make([]string, 0, len(toRaw))
			for _, r := range toRaw {
				s, ok := r.(string)
				if !ok || s == "" {
					return nil, fmt.Errorf("missing required input: to ([]string)")
				}
				recipients = append(recipients, s)
			}
			subject, ok := ctx.Input["subject"].(string)
			if !ok || subject == "" {
				return nil, fmt.Errorf("missing required input: subject (string)")
			}
			body, ok := ctx.Input["body"].(string)
			if !ok || body == "" {
				return nil, fmt.Errorf("missing required input: body (string)")
			}
			html, _ := ctx.Input["html"].(bool)

			contentType := "text/plain; charset=utf-8"
			if html {
				contentType = "text/html; charset=utf-8"
			}

			// RFC 5322 message: headers, a blank line, then the body. CRLF
			// line endings throughout — what SMTP transports. From/To/Subject
			// are the envelope-independent display headers; the actual
			// envelope (MAIL FROM / RCPT TO) is what SendMail sends from the
			// `from` and `recipients` args, not from these headers.
			msg := strings.Join([]string{
				"From: " + from,
				"To: " + strings.Join(recipients, ", "),
				"Subject: " + subject,
				"MIME-Version: 1.0",
				"Content-Type: " + contentType,
				"",
				body,
			}, "\r\n")

			// Auth is optional: only when ctx.Auth is a "user:password"
			// string (split on the FIRST colon, so a password containing a
			// colon is preserved) do we authenticate. Anything else — no
			// auth, a non-string, or a string without a colon — means send
			// unauthenticated (auth=nil), a valid open-relay / no-auth case
			// that must not fail the request. smtp.PlainAuth refuses to send
			// credentials over a non-TLS connection unless the host is
			// localhost-ish; the host here is the part of smtpAddr before
			// ":port" (net.SplitHostPort), which in tests is 127.0.0.1, so
			// that check passes without STARTTLS.
			var auth smtp.Auth
			if s, ok := ctx.Auth.(string); ok {
				if idx := strings.Index(s, ":"); idx >= 0 {
					if host, _, err := net.SplitHostPort(smtpAddr); err == nil {
						auth = smtp.PlainAuth("", s[:idx], s[idx+1:], host)
					}
				}
			}

			if err := smtp.SendMail(smtpAddr, auth, from, recipients, []byte(msg)); err != nil {
				return nil, fmt.Errorf("email: sending: %w", err)
			}

			return map[string]any{
				"sent": true,
				"to":   len(recipients),
			}, nil
		},
	}
}
