package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/7ar-chat/harness/core/netguard"
)

// Message is what a sender delivers.
type Message struct {
	Event       Kind           `json:"event"`
	ProjectID   string         `json:"-"`
	ProjectName string         `json:"project"`
	Title       string         `json:"title"`
	Text        string         `json:"-"`
	URL         string         `json:"url"`
	At          time.Time      `json:"at"`
	Data        map[string]any `json:"data"`
	// Test marks the message of a Test button.
	Test bool `json:"-"`
}

// Target is where a sender delivers: the opened address and secret of a channel, or the person of a personal channel.
type Target struct {
	URL    string
	Secret string
	// Login is the person for Telegram, Slack and email, whose address the sender looks up itself.
	Login string
}

// Sender delivers one message. Errors it returns are shown to people: they must not contain the address or secret.
type Sender interface {
	Send(ctx context.Context, t Target, m Message) error
}

// SenderFunc adapts a function to Sender.
type SenderFunc func(ctx context.Context, t Target, m Message) error

// Send implements Sender.
func (f SenderFunc) Send(ctx context.Context, t Target, m Message) error { return f(ctx, t, m) }

// HTTPSender posts JSON to an address a user supplied. One type serves the three kinds; Kind picks the payload.
type HTTPSender struct {
	// Kind is KindWebhook, KindWeCom or KindDiscord.
	Kind   string
	Client *http.Client
	// Attempts is the number of tries (default 3); Backoff the wait before the second (default 1s, doubling).
	Attempts int
	Backoff  time.Duration
	// Sleep replaces the wait (tests).
	Sleep func(time.Duration)
}

// NewHTTPClient returns the client of the senders: SSRF guarded (see core/netguard) unless the operator allowed
// private addresses, limited redirects, a timeout per try.
func NewHTTPClient(allowPrivate bool) *http.Client {
	return &http.Client{Transport: netguard.Transport(allowPrivate), Timeout: 15 * time.Second, CheckRedirect: netguard.SafeCheckRedirect}
}

// SignatureHeader is the header that carries the HMAC-SHA256 of a webhook body: "sha256=" and the hex digest.
const SignatureHeader = "X-7ar-Signature"

// Sign returns the value of SignatureHeader for body under secret.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// errClass is a fixed, address-free description of a failure to reach a server.
func errClass(err error) string {
	switch {
	case err == nil:
		return ""
	case strings.Contains(err.Error(), "destination not allowed"):
		return "the address is not allowed (private or internal addresses are refused)"
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "Timeout") || strings.Contains(err.Error(), "timeout"):
		return "the server did not answer in time"
	case strings.Contains(err.Error(), "redirect"):
		return "the server redirected somewhere that is not allowed"
	case strings.Contains(err.Error(), "certificate") || strings.Contains(err.Error(), "tls:"):
		return "the server's certificate was refused"
	}
	return "could not connect to the server"
}

type retryable struct{ msg string }

func (r retryable) Error() string { return r.msg }

// Send implements Sender.
func (s *HTTPSender) Send(ctx context.Context, t Target, m Message) error {
	if t.URL == "" {
		return errors.New("no address")
	}
	u, err := url.Parse(t.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("the address is not a web address")
	}
	body, hdr, err := s.payload(t, m)
	if err != nil {
		return err
	}
	attempts, wait := s.Attempts, s.Backoff
	if attempts <= 0 {
		attempts = 3
	}
	if wait <= 0 {
		wait = time.Second
	}
	sleep := s.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			sleep(wait)
			wait *= 2
		}
		err := s.once(ctx, t.URL, body, hdr)
		if err == nil {
			return nil
		}
		last = err
		var r retryable
		if !errors.As(err, &r) || ctx.Err() != nil {
			break
		}
	}
	return errors.New(last.Error())
}

func (s *HTTPSender) once(ctx context.Context, addr string, body []byte, hdr http.Header) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addr, bytes.NewReader(body))
	if err != nil {
		return errors.New("the address is not a web address")
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	c := s.Client
	if c == nil {
		c = NewHTTPClient(false)
	}
	resp, err := c.Do(req)
	if err != nil {
		msg := errClass(err)
		if strings.Contains(msg, "not allowed") || strings.Contains(msg, "certificate") {
			return errors.New(msg)
		}
		return retryable{msg}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return errors.New("the server redirected somewhere that is not allowed")
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return retryable{fmt.Sprintf("the server answered %d", resp.StatusCode)}
	case resp.StatusCode >= 400:
		return fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	if s.Kind == KindWeCom {
		var out struct {
			Code int    `json:"errcode"`
			Msg  string `json:"errmsg"`
		}
		if json.Unmarshal(b, &out) == nil && out.Code != 0 {
			return fmt.Errorf("WeCom refused the message (code %d)", out.Code)
		}
	}
	return nil
}

func (s *HTTPSender) payload(t Target, m Message) ([]byte, http.Header, error) {
	hdr := http.Header{"Content-Type": {"application/json"}, "User-Agent": {"7ar-notifications"}}
	var v any
	switch s.Kind {
	case KindWeCom:
		v = map[string]any{"msgtype": "markdown", "markdown": map[string]string{"content": WeComMarkdown(m)}}
	case KindDiscord:
		v = map[string]any{"username": "7ar", "content": DiscordText(m), "allowed_mentions": map[string]any{"parse": []string{}}}
	default:
		data := m.Data
		if data == nil {
			data = map[string]any{}
		}
		if m.Text != "" {
			data = copyMap(data)
			data["body"] = m.Text
		}
		at := m.At
		if at.IsZero() {
			at = time.Now()
		}
		v = map[string]any{"event": string(m.Event), "project": m.ProjectName, "title": m.Title, "url": m.URL, "at": at.UTC().Format(time.RFC3339), "data": data}
		hdr.Set("X-7ar-Event", string(m.Event))
	}
	body, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	if s.Kind == KindWebhook || s.Kind == "" {
		if t.Secret != "" {
			hdr.Set(SignatureHeader, Sign(t.Secret, body))
		}
	}
	return body, hdr, nil
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func headline(m Message) string {
	h := m.Title
	if m.Test {
		h = "[TEST] " + h
	}
	if m.ProjectName != "" {
		h = "[" + m.ProjectName + "] " + h
	}
	return h
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && s[len(s)-1]&0xC0 == 0x80 { // do not cut a character in half
		s = s[:len(s)-1]
	}
	if len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// WeComMarkdown is the markdown a WeCom group bot message carries (limit 4096 bytes).
func WeComMarkdown(m Message) string {
	var b strings.Builder
	b.WriteString("**" + strings.NewReplacer("\n", " ", "*", "").Replace(headline(m)) + "**")
	if m.Text != "" {
		b.WriteString("\n" + m.Text)
	}
	if m.URL != "" {
		b.WriteString("\n[Open in 7ar](" + m.URL + ")")
	}
	return clip(b.String(), 4000)
}

// DiscordText is the content of a Discord webhook message (limit 2000 characters).
func DiscordText(m Message) string {
	var b strings.Builder
	b.WriteString("**" + strings.NewReplacer("\n", " ", "*", "").Replace(headline(m)) + "**")
	if m.Text != "" {
		b.WriteString("\n" + m.Text)
	}
	if m.URL != "" {
		b.WriteString("\n" + m.URL)
	}
	return clip(b.String(), 1900)
}

// ValidateAddress checks the address of a new channel of kind. strict also pins WeCom and Discord to their servers
// (an operator who allows private addresses relaxes that, for a proxy or a test).
func ValidateAddress(kind, raw string, strict bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("enter a full web address that starts with https://")
	}
	if u.User != nil {
		return errors.New("the address must not contain a user name or password")
	}
	if !strict {
		return nil
	}
	if u.Scheme != "https" {
		return errors.New("the address must start with https://")
	}
	host := strings.ToLower(u.Hostname())
	switch kind {
	case KindWeCom:
		if host != "qyapi.weixin.qq.com" || !strings.HasPrefix(u.Path, "/cgi-bin/webhook/send") || u.Query().Get("key") == "" {
			return errors.New("a WeCom bot address looks like https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...")
		}
	case KindDiscord:
		if (host != "discord.com" && host != "discordapp.com" && !strings.HasSuffix(host, ".discord.com")) || !strings.HasPrefix(u.Path, "/api/webhooks/") {
			return errors.New("a Discord webhook address looks like https://discord.com/api/webhooks/...")
		}
	}
	return nil
}

// HostHint is what may be shown of an address: its host (and for WeCom and Discord the fixed part of the path).
func HostHint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}
