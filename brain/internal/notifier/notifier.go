// Package notifier defines the Notifier interface and provides a production
// SMTP implementation alongside a NopNotifier for use when alerts are disabled.
package notifier

import (
	"crypto/tls"
	"fmt"
	"net/mail"
	"net/smtp"
	"os"
	"strings"
)

// Notifier is the delivery contract for alert messages.
// Any transport (email, Slack, webhook) implements this single method.
type Notifier interface {
	Send(subject, body string) error
}

// ── EmailNotifier ─────────────────────────────────────────────────────────────

// EmailConfig holds all parameters needed to connect and authenticate with an
// SMTP server and route the outbound message.
type EmailConfig struct {
	Host      string // e.g. "smtp.gmail.com"
	Port      int    // e.g. 587 for STARTTLS, 465 for TLS
	Username  string // SMTP auth username
	Password  string // SMTP auth password
	From      string // envelope From address; defaults to Username when empty
	Recipient string // destination address
}

// EmailNotifier sends alerts via SMTP PLAIN auth with STARTTLS.
// smtp.SendMail upgrades to TLS before transmitting credentials when the
// server advertises the STARTTLS capability.
type EmailNotifier struct {
	cfg EmailConfig
}

func NewEmailNotifier(cfg EmailConfig) *EmailNotifier {
	return &EmailNotifier{cfg: cfg}
}

func (e *EmailNotifier) Send(subject, body string) error {
	from := e.cfg.From
	if from == "" {
		from = e.cfg.Username
	}

	// Build a minimal RFC 5322 message.
	var msg strings.Builder
	fmt.Fprintf(&msg, "From: %s\r\n", from)
	fmt.Fprintf(&msg, "To: %s\r\n", e.cfg.Recipient)
	fmt.Fprintf(&msg, "Subject: %s\r\n", subject)
	msg.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("\r\n")
	msg.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))

	addr := fmt.Sprintf("%s:%d", e.cfg.Host, e.cfg.Port)

	// Use a manual SMTP flow so we can call Hello() with the real local
	// hostname. smtp.SendMail sends "EHLO localhost" by default, which
	// Gmail rejects with a 555 syntax error.
	c, err := smtp.Dial(addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.Close()

	localHost, err := os.Hostname()
	if err != nil || localHost == "" {
		localHost = "mail.local"
	}
	if err := c.Hello(localHost); err != nil {
		return fmt.Errorf("EHLO: %w", err)
	}
	if err := c.StartTLS(&tls.Config{ServerName: e.cfg.Host}); err != nil {
		return fmt.Errorf("STARTTLS: %w", err)
	}
	auth := smtp.PlainAuth("", e.cfg.Username, e.cfg.Password, e.cfg.Host)
	if err := c.Auth(auth); err != nil {
		return fmt.Errorf("AUTH: %w", err)
	}
	// SMTP envelope needs a bare address; strip any display name.
	envelopeFrom := from
	if addr, err := mail.ParseAddress(from); err == nil {
		envelopeFrom = addr.Address
	}
	if err := c.Mail(envelopeFrom); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	if err := c.Rcpt(e.cfg.Recipient); err != nil {
		return fmt.Errorf("RCPT TO: %w", err)
	}
	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := fmt.Fprint(wc, msg.String()); err != nil {
		wc.Close()
		return fmt.Errorf("write body: %w", err)
	}
	return wc.Close()
}

// ── NopNotifier ───────────────────────────────────────────────────────────────

// NopNotifier is a null-object implementation. It silently discards every
// alert. Use it when notifications.enabled = false in config.yaml so the rest
// of the code never needs to nil-check the Notifier.
type NopNotifier struct{}

func (NopNotifier) Send(_, _ string) error { return nil }
