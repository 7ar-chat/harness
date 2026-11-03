package product

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
)

// fakeSMTP accepts one message and records what the client said.
type fakeSMTP struct {
	ln    net.Listener
	lines chan string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, lines: make(chan string, 100)}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		rd := bufio.NewReader(c)
		say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
		say("220 fake ESMTP")
		data := false
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			f.lines <- line
			if data {
				if line == "." {
					data = false
					say("250 queued")
				}
				continue
			}
			switch cmd := strings.ToUpper(line); {
			case strings.HasPrefix(cmd, "EHLO"):
				say("250-fake\r\n250 AUTH PLAIN")
			case strings.HasPrefix(cmd, "AUTH PLAIN"):
				say("235 ok")
			case strings.HasPrefix(cmd, "DATA"):
				data = true
				say("354 go")
			case strings.HasPrefix(cmd, "QUIT"):
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) all() string {
	close(f.lines)
	var b strings.Builder
	for l := range f.lines {
		b.WriteString(l + "\n")
	}
	return b.String()
}

func TestSMTPMailerSendsOneMessageToEveryone(t *testing.T) {
	// Arrange
	srv := newFakeSMTP(t)
	m, err := NewSMTPMailer(SMTPConfig{Host: "127.0.0.1", Port: srv.port(), Username: "u", Password: "p", From: "Harness <noreply@x.io>"})
	if err != nil {
		t.Fatal(err)
	}

	// Act: a subject with a line break must not inject a header.
	err = m.Send(context.Background(), []string{"a@x.io", "B <b@x.io>"}, "New sign-up\r\nBcc: evil@x.io", "line one\nline two")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	got := srv.all()
	for _, want := range []string{"MAIL FROM:<noreply@x.io>", "RCPT TO:<a@x.io>", "RCPT TO:<b@x.io>", "From: Harness <noreply@x.io>",
		"To: a@x.io, b@x.io", "Subject: New sign-up  Bcc: evil@x.io", "line one", "line two", "AUTH PLAIN"} {
		if !strings.Contains(got, want) {
			t.Errorf("the conversation lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\nBcc:") {
		t.Errorf("a header was injected:\n%s", got)
	}
}

func TestSMTPMailerRejectsBadConfigurationAndRecipients(t *testing.T) {
	if _, err := NewSMTPMailer(SMTPConfig{From: "a@x.io"}); err == nil {
		t.Error("no host accepted")
	}
	if _, err := NewSMTPMailer(SMTPConfig{Host: "h", From: "not an address"}); err == nil {
		t.Error("a bad sender accepted")
	}
	m, _ := NewSMTPMailer(SMTPConfig{Host: "127.0.0.1", Port: 1, From: "a@x.io"})
	if err := m.Send(context.Background(), nil, "s", "b"); err == nil {
		t.Error("no recipient accepted")
	}
	if err := m.Send(context.Background(), []string{"nope"}, "s", "b"); err == nil {
		t.Error("a bad recipient accepted")
	}
	if err := m.Send(context.Background(), []string{"a@x.io"}, "s", "b"); err == nil || !strings.Contains(err.Error(), strconv.Itoa(1)) {
		t.Errorf("an unreachable server: %v", err)
	}
}
