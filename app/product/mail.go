package product

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Mailer sends the product's emails (the notice to the admins when someone signs up).
type Mailer interface {
	Send(ctx context.Context, to []string, subject, body string) error
}

// SMTPConfig is the mail server the notices go through.
type SMTPConfig struct {
	Host string
	// Port defaults to 587 (STARTTLS when the server offers it); 465 connects over TLS straight away.
	Port     int
	Username string
	Password string
	// From is the sender address, "Harness <noreply@example.com>" or a bare address.
	From string
}

// SMTPMailer is a Mailer over SMTP.
type SMTPMailer struct {
	cfg  SMTPConfig
	from string // the bare sender address
}

// NewSMTPMailer validates the configuration.
func NewSMTPMailer(cfg SMTPConfig) (*SMTPMailer, error) {
	if cfg.Host == "" {
		return nil, errors.New("smtp: no host")
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	a, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return nil, fmt.Errorf("smtp: the sender (SMTP_FROM) is not an address: %w", err)
	}
	return &SMTPMailer{cfg: cfg, from: a.Address}, nil
}

// clean keeps a header value on one line: a login or a name must not be able to add headers.
func clean(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// Send delivers the message to every recipient in one transaction.
func (m *SMTPMailer) Send(ctx context.Context, to []string, subject, body string) error {
	if len(to) == 0 {
		return errors.New("smtp: no recipient")
	}
	var rcpt []string
	for _, t := range to {
		a, err := mail.ParseAddress(t)
		if err != nil {
			return fmt.Errorf("smtp: recipient %q: %w", t, err)
		}
		rcpt = append(rcpt, a.Address)
	}
	addr := net.JoinHostPort(m.cfg.Host, strconv.Itoa(m.cfg.Port))
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	if m.cfg.Port == 465 {
		conn = tls.Client(conn, &tls.Config{ServerName: m.cfg.Host, MinVersion: tls.VersionTLS12})
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok && m.cfg.Port != 465 {
		if err := c.StartTLS(&tls.Config{ServerName: m.cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if m.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(m.from); err != nil {
		return err
	}
	for _, r := range rcpt {
		if err := c.Rcpt(r); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	msg := "From: " + clean(m.cfg.From) + "\r\n" +
		"To: " + clean(strings.Join(rcpt, ", ")) + "\r\n" +
		"Subject: " + mime.QEncoding.Encode("utf-8", clean(subject)) + "\r\n" +
		"Date: " + time.Now().Format(time.RFC1123Z) + "\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" +
		strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n") + "\r\n"
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
