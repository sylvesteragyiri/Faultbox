// Package redismon polls configured Redis instances using the small RESP2
// subset needed for AUTH, SELECT, and INFO. It intentionally has no client
// dependency and never reads keys or values.
package redismon

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// Interval is the time between checks; DialTimeout bounds each connection.
const (
	Interval    = 5 * time.Minute
	DialTimeout = 3 * time.Second
	readLimit   = 1 << 20
	pollWorkers = 4
)

// Collector polls targets at a fixed interval. Old samples are deleted by
// the retention sweep (store.Sweep), with the issue retention period.
type Collector struct {
	store  *store.Store
	key    []byte
	logger interface{ Error(string, ...any) }
	alerts interface {
		RedisMemoryAlert(context.Context, string, int64, int64, int64, int64) error
	}
	now func() time.Time
}

// New returns a collector that unseals endpoints with key.
func New(st *store.Store, key []byte, logger interface{ Error(string, ...any) }) *Collector {
	return &Collector{store: st, key: key, logger: logger, now: time.Now}
}

// SetAlertSender connects threshold crossings to the durable notification queue.
func (c *Collector) SetAlertSender(sender interface {
	RedisMemoryAlert(context.Context, string, int64, int64, int64, int64) error
}) {
	c.alerts = sender
}

// Run polls immediately on startup and then every five minutes until stopped.
func (c *Collector) Run(ctx context.Context) {
	_ = c.Poll(ctx)
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = c.Poll(ctx)
		}
	}
}

// Poll performs one check for each enabled target. It is exposed for focused
// tests and operational diagnostics.
func (c *Collector) Poll(ctx context.Context) error {
	targets, err := c.store.RedisTargets(ctx)
	if err != nil {
		return fmt.Errorf("list Redis targets: %w", err)
	}
	var errs []error
	jobs := make(chan store.RedisTarget)
	results := make(chan error, len(targets))
	var wg sync.WaitGroup
	for range min(pollWorkers, len(targets)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for target := range jobs {
				if target.Enabled {
					if err := c.pollTarget(ctx, target); err != nil {
						results <- err
					}
				}
			}
		}()
	}
	for _, target := range targets {
		select {
		case jobs <- target:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			close(results)
			for err := range results {
				errs = append(errs, err)
			}
			return errors.Join(append(errs, ctx.Err())...)
		}
	}
	close(jobs)
	wg.Wait()
	close(results)
	for err := range results {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (c *Collector) pollTarget(ctx context.Context, target store.RedisTarget) error {
	plain, err := auth.Unseal(c.key, target.Endpoint)
	var sample store.RedisSample
	if err != nil {
		err = errors.New("the saved Redis URL could not be decrypted; FAULTBOX_SECRET may have changed, so enter the URL again")
	} else {
		sample, err = ReadInfo(ctx, string(plain))
	}
	// The target ID is needed on failure too: RecordRedisCheck uses it to
	// store the failure reason and check time.
	sample.TargetID, sample.TargetName = target.ID, target.Name
	checkErr := ""
	if err != nil {
		checkErr = Reason(err)
		if c.logger != nil {
			c.logger.Error("Redis monitoring check failed", "target_id", target.ID, "error", checkErr)
		}
	} else {
		sample.SampledAt = c.now().UTC()
	}
	var previous store.RedisSample
	if err == nil && target.AlertEnabled {
		var previousErr error
		previous, previousErr = c.store.LatestRedisSample(ctx, target.ID)
		if previousErr != nil && !errors.Is(previousErr, store.ErrNotFound) {
			return fmt.Errorf("load previous Redis sample for target %d: %w", target.ID, previousErr)
		}
	}
	if dbErr := c.store.RecordRedisCheck(ctx, sample, checkErr); dbErr != nil {
		return fmt.Errorf("record Redis target %d check: %w", target.ID, dbErr)
	}
	if err != nil {
		return fmt.Errorf("redis target %d: %w", target.ID, err)
	}
	if target.AlertEnabled && c.alerts != nil && sample.MaxMemory > 0 {
		pct := memoryPercent(sample.UsedMemory, sample.MaxMemory)
		if threshold := crossedThreshold(memoryPercent(previous.UsedMemory, previous.MaxMemory), pct); threshold > 0 {
			if alertErr := c.alerts.RedisMemoryAlert(ctx, target.Name, sample.UsedMemory, sample.MaxMemory, pct, threshold); alertErr != nil {
				return fmt.Errorf("queue Redis alert for target %d at %d%%: %w", target.ID, threshold, alertErr)
			}
		}
	}
	return nil
}

// AlertThresholds are the memory percentages that raise an alert when a
// check moves from below to at-or-above them.
var AlertThresholds = []int64{80, 90, 100}

// crossedThreshold returns the highest threshold crossed between two checks,
// or 0. A jump from 70% to 100% sends one alert for 100%, not three.
func crossedThreshold(previous, current int64) int64 {
	crossed := int64(0)
	for _, threshold := range AlertThresholds {
		if previous < threshold && current >= threshold {
			crossed = threshold
		}
	}
	return crossed
}

func memoryPercent(used, maximum int64) int64 {
	if maximum <= 0 || used <= 0 {
		return 0
	}
	return int64(float64(used) * 100 / float64(maximum))
}

// Reason turns a check error into a short, single-line sentence suitable for
// the dashboard, logs, and the stored last error. ReadInfo errors are already
// fixed strings; this also bounds anything else that reaches it.
func Reason(err error) string {
	if err == nil {
		return ""
	}
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 180 {
		s = strings.ToValidUTF8(s[:180], "")
	}
	if s == "" {
		return "Redis check failed."
	}
	first, size := utf8.DecodeRuneInString(s)
	s = string(unicode.ToUpper(first)) + s[size:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// ReadInfo opens one Redis connection and reads bounded INFO data.
func ReadInfo(ctx context.Context, endpoint string) (store.RedisSample, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "redis" && u.Scheme != "rediss") || u.Hostname() == "" {
		return store.RedisSample{}, errors.New("endpoint must be a redis:// or rediss:// URL")
	}
	port := u.Port()
	if port == "" {
		port = "6379"
	}
	address := net.JoinHostPort(u.Hostname(), port)
	dialer := net.Dialer{Timeout: DialTimeout}
	var conn net.Conn
	if u.Scheme == "rediss" {
		tlsDialer := tls.Dialer{NetDialer: &dialer, Config: &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}}
		conn, err = tlsDialer.DialContext(ctx, "tcp", address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return store.RedisSample{}, dialError(err)
	}
	defer func() { _ = conn.Close() }()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReaderSize(conn, 4096)
	w := bufio.NewWriter(conn)
	if password, hasPassword := u.User.Password(); hasPassword {
		username := u.User.Username()
		var reply string
		if username != "" {
			reply, err = command(r, w, "AUTH", username, password)
		} else {
			reply, err = command(r, w, "AUTH", password)
		}
		if err != nil || reply != "OK" {
			return store.RedisSample{}, authError(err)
		}
	}
	if db := strings.TrimPrefix(u.Path, "/"); db != "" && db != "0" {
		if n, err := strconv.Atoi(db); err != nil || n < 0 {
			return store.RedisSample{}, errors.New("redis database path must be a number")
		}
		if _, err := command(r, w, "SELECT", db); err != nil {
			return store.RedisSample{}, commandError(err, "SELECT", "redis database selection failed")
		}
	}
	info, err := command(r, w, "INFO")
	if err != nil {
		return store.RedisSample{}, commandError(err, "INFO", "redis INFO failed")
	}
	return parseInfo(info)
}

// replyError is a Redis error reply. Its Error text is fixed because the
// server's message may echo user data; callers map Code to a safe reason.
type replyError struct{ Code, Message string }

func (e *replyError) Error() string { return "Redis returned an error" }

// protocolError is a fixed, already-safe description of a malformed reply.
type protocolError string

func (e protocolError) Error() string { return string(e) }

// Failure reasons are fixed strings so stored errors and logs never carry
// hostnames, addresses, credentials, or server-supplied text.
var (
	errTimeout = errors.New("redis did not respond in time")
	errClosed  = errors.New("redis closed the connection")
)

func dialError(err error) error {
	var dnsErr *net.DNSError
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalidCert x509.CertificateInvalidError
	var verifyErr *tls.CertificateVerificationError
	var recordErr tls.RecordHeaderError
	switch {
	case errors.As(err, &dnsErr):
		return errors.New("redis host name could not be resolved")
	case isTimeout(err):
		return errors.New("connection to Redis timed out")
	case isRefused(err):
		return errors.New("redis refused the connection; check the host and port")
	case errors.As(err, &verifyErr), errors.As(err, &unknownCA), errors.As(err, &hostErr), errors.As(err, &invalidCert):
		return errors.New("redis TLS certificate could not be verified")
	case errors.As(err, &recordErr):
		return errors.New("redis did not answer with TLS; use redis:// for plain connections")
	case errors.Is(err, io.EOF), isReset(err):
		return errors.New("redis closed the connection during the TLS handshake")
	}
	return errors.New("could not connect to Redis")
}

func authError(err error) error {
	var reply *replyError
	if errors.As(err, &reply) {
		if strings.Contains(strings.ToLower(reply.Message), "without any password configured") {
			return errors.New("redis has no password set; remove the password from the URL")
		}
		return errors.New("redis rejected the username or password")
	}
	if err == nil {
		return errors.New("redis authentication failed")
	}
	return transportError(err)
}

func commandError(err error, name, fallback string) error {
	var reply *replyError
	if errors.As(err, &reply) {
		switch reply.Code {
		case "NOAUTH":
			return errors.New("redis requires a password; add it to the URL")
		case "WRONGPASS":
			return errors.New("redis rejected the username or password")
		case "NOPERM":
			return fmt.Errorf("the Redis user is not allowed to run %s", name)
		case "LOADING":
			return errors.New("redis is still loading its dataset")
		}
		if name == "SELECT" && strings.Contains(strings.ToLower(reply.Message), "out of range") {
			return errors.New("redis database number is out of range")
		}
		return errors.New(fallback)
	}
	return transportError(err)
}

func transportError(err error) error {
	var proto protocolError
	switch {
	case errors.As(err, &proto):
		return proto
	case isTimeout(err):
		return errTimeout
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), isReset(err):
		return errClosed
	}
	return errors.New("redis connection failed")
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout())
}

// Windows reports socket errors as WSA codes rather than POSIX errno values.
const (
	wsaConnReset   syscall.Errno = 10054
	wsaConnRefused syscall.Errno = 10061
)

func isRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, wsaConnRefused)
}

func isReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, wsaConnReset)
}

func command(r *bufio.Reader, w *bufio.Writer, args ...string) (string, error) {
	if _, err := fmt.Fprintf(w, "*%d\r\n", len(args)); err != nil {
		return "", err
	}
	for _, arg := range args {
		if _, err := fmt.Fprintf(w, "$%d\r\n%s\r\n", len(arg), arg); err != nil {
			return "", err
		}
	}
	if err := w.Flush(); err != nil {
		return "", err
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	if len(line) < 3 {
		return "", protocolError("invalid Redis response")
	}
	switch line[0] {
	case '+':
		return strings.TrimSuffix(strings.TrimSuffix(line[1:], "\n"), "\r"), nil
	case '-':
		code, message, _ := strings.Cut(strings.TrimSpace(line[1:]), " ")
		return "", &replyError{Code: code, Message: message}
	case '$':
		n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
		if err != nil || n < 0 || n > readLimit {
			return "", protocolError("redis INFO response exceeds limit")
		}
		b := make([]byte, n+2)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		if b[n] != '\r' || b[n+1] != '\n' {
			return "", protocolError("invalid Redis bulk response")
		}
		return string(b[:n]), nil
	default:
		return "", protocolError("unexpected Redis response; if the server requires TLS, use rediss://")
	}
}

func parseInfo(raw string) (store.RedisSample, error) {
	values := make(map[string]string)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.IndexByte(line, ':'); i > 0 {
			values[line[:i]] = line[i+1:]
		}
	}
	read := func(key string) (int64, error) {
		v, ok := values[key]
		if !ok {
			return 0, fmt.Errorf("redis INFO is missing %s", key)
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("redis INFO has invalid %s", key)
		}
		return n, nil
	}
	var s store.RedisSample
	var err error
	if s.UsedMemory, err = read("used_memory"); err != nil {
		return s, err
	}
	if s.MaxMemory, err = read("maxmemory"); err != nil {
		return s, err
	}
	if s.ConnectedClients, err = read("connected_clients"); err != nil {
		return s, err
	}
	if s.UptimeSeconds, err = read("uptime_in_seconds"); err != nil {
		return s, err
	}
	for key, value := range values {
		if !strings.HasPrefix(key, "db") {
			continue
		}
		for _, field := range strings.Split(value, ",") {
			k, v, ok := strings.Cut(field, "=")
			if !ok || k != "keys" {
				continue
			}
			n, e := strconv.ParseInt(v, 10, 64)
			if e != nil || n < 0 {
				return s, errors.New("redis INFO has invalid key count")
			}
			s.KeyCount += n
		}
	}
	return s, nil
}
