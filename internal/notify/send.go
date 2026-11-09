package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// ---- webhooks ----

// Webhook request headers. Receivers verify a delivery by computing
// HMAC-SHA256(secret, timestamp + "." + body) and comparing it, in
// constant time, with the hex value after "sha256=" in SignatureHeader.
// They should also reject timestamps more than a few minutes old.
const (
	SignatureHeader = "X-Faultbox-Signature"
	TimestampHeader = "X-Faultbox-Timestamp"
	EventHeader     = "X-Faultbox-Event"
	DeliveryHeader  = "X-Faultbox-Delivery"
)

// Sign returns the signature header value for a webhook body.
func Sign(secret []byte, timestamp int64, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(strconv.FormatInt(timestamp, 10)))
	m.Write([]byte("."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// NewWebhookSecret generates a signing secret for a webhook channel.
func NewWebhookSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "whsec_" + hex.EncodeToString(b), nil
}

func (n *Notifier) sendWebhook(ctx context.Context, c store.Channel, secret []byte, msg Message) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return permanent(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Config.URL, bytes.NewReader(body))
	if err != nil {
		return permanent(err)
	}
	ts := n.now().Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Faultbox/"+n.opts.Version)
	req.Header.Set(EventHeader, msg.Trigger)
	req.Header.Set(DeliveryHeader, strconv.FormatInt(msg.DeliveryID, 10))
	req.Header.Set(TimestampHeader, strconv.FormatInt(ts, 10))
	if len(secret) > 0 {
		req.Header.Set(SignatureHeader, Sign(secret, ts, body))
	}
	resp, err := n.opts.HTTPClient.Do(req) //nolint:gosec // G704: the URL is an admin-configured webhook target
	if err != nil {
		return fmt.Errorf("webhook request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	err = fmt.Errorf("webhook answered %s: %s", resp.Status, oneLine(string(snippet), 200))
	// Client errors won't fix themselves, except timeouts and rate limits.
	if resp.StatusCode >= 400 && resp.StatusCode < 500 &&
		resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
		return permanent(err)
	}
	return err
}

// ---- email ----

// Mailer delivers one email message. msg is the complete RFC 5322 message.
type Mailer interface {
	Send(ctx context.Context, cfg store.ChannelConfig, password string, from string, to []string, msg []byte) error
}

func (n *Notifier) sendEmail(ctx context.Context, c store.Channel, password string, msg Message) error {
	from, err := mail.ParseAddress(c.Config.From)
	if err != nil {
		return permanent(fmt.Errorf("invalid From address: %w", err))
	}
	raw := buildEmail(from, c.Config.To, msg, n.now())
	return n.opts.Mailer.Send(ctx, c.Config, password, from.Address, c.Config.To, raw)
}

// buildEmail renders a plain-text, quoted-printable message.
func buildEmail(from *mail.Address, to []string, msg Message, now time.Time) []byte {
	var b bytes.Buffer
	header := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	header("From", from.String())
	header("To", strings.Join(to, ", "))
	header("Subject", mime.QEncoding.Encode("utf-8", oneLine(msg.Subject, 200)))
	header("Date", now.UTC().Format(time.RFC1123Z))
	header("Message-ID", fmt.Sprintf("<faultbox.%d.%d@%s>", msg.DeliveryID, now.UnixNano(), domainOf(from.Address)))
	header("MIME-Version", "1.0")
	header("Content-Type", `text/plain; charset="utf-8"`)
	header("Content-Transfer-Encoding", "quoted-printable")
	header("Auto-Submitted", "auto-generated")
	header("X-Faultbox-Event", msg.Trigger)
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	_, _ = qp.Write([]byte(strings.ReplaceAll(msg.text(), "\n", "\r\n")))
	_ = qp.Close()
	return b.Bytes()
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return addr[i+1:]
	}
	return "faultbox.local"
}

// SMTPMailer sends through the SMTP server in the channel's settings.
type SMTPMailer struct {
	// Timeout bounds the whole conversation. Zero means 30 seconds.
	Timeout time.Duration
}

// Send implements Mailer.
func (m SMTPMailer) Send(ctx context.Context, cfg store.ChannelConfig, password, from string, to []string, msg []byte) error {
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	tlsConfig := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}

	var (
		conn net.Conn
		err  error
	)
	if cfg.Security == store.SMTPTLS {
		conn, err = (&tls.Dialer{Config: tlsConfig}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("SMTP greeting from %s: %w", addr, err)
	}
	defer func() { _ = c.Close() }()

	if cfg.Security == store.SMTPStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return permanent(errors.New("the SMTP server doesn't offer STARTTLS; choose TLS or no encryption"))
		}
		if err := c.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}
	if cfg.Username != "" {
		// PlainAuth refuses to send credentials over an unencrypted
		// connection to anything but localhost.
		if err := c.Auth(smtp.PlainAuth("", cfg.Username, password, cfg.Host)); err != nil {
			return smtpError("authenticate", err)
		}
	}
	if err := c.Mail(from); err != nil {
		return smtpError("MAIL FROM", err)
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return smtpError("RCPT TO "+rcpt, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return smtpError("DATA", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return smtpError("send message", err)
	}
	return c.Quit()
}

// smtpError marks 5xx replies as permanent: the server has refused for good.
func smtpError(step string, err error) error {
	err = fmt.Errorf("%s: %w", step, err)
	var tp *textproto.Error
	if errors.As(err, &tp) && tp.Code >= 500 {
		return permanent(err)
	}
	return err
}
