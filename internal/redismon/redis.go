// Package redismon polls configured Redis instances using the small RESP2
// subset needed for AUTH, SELECT, and INFO. It intentionally has no client
// dependency and never reads keys or values.
package redismon

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/internal/store"
)

const (
	Interval    = 5 * time.Minute
	DialTimeout = 3 * time.Second
	readLimit   = 1 << 20
	pollWorkers = 4
)

// Collector polls targets at a fixed interval and prunes samples using the
// configured global retention period.
type Collector struct {
	store         *store.Store
	key           []byte
	retentionDays int
	logger        interface{ Error(string, ...any) }
	alerts        interface {
		RedisMemoryAlert(context.Context, string, int64, int64, int64, int64) error
	}
	now func() time.Time
}

func New(st *store.Store, key []byte, retentionDays int, logger interface{ Error(string, ...any) }) *Collector {
	if retentionDays < 1 {
		retentionDays = 30
	}
	return &Collector{store: st, key: key, retentionDays: retentionDays, logger: logger, now: time.Now}
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
	if err := c.store.PruneRedisSamples(ctx, c.now().AddDate(0, 0, -c.retentionDays)); err != nil {
		errs = append(errs, fmt.Errorf("prune Redis history: %w", err))
	}
	return errors.Join(errs...)
}

func (c *Collector) pollTarget(ctx context.Context, target store.RedisTarget) error {
	plain, err := auth.Unseal(c.key, target.Endpoint)
	var sample store.RedisSample
	checkErr := ""
	if err == nil {
		sample, err = ReadInfo(ctx, string(plain))
	}
	if err != nil {
		checkErr = "Redis check failed: " + safeError(err)
		if c.logger != nil {
			c.logger.Error("Redis monitoring check failed", "target_id", target.ID, "error", checkErr)
		}
	} else {
		sample.TargetID, sample.TargetName = target.ID, target.Name
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
		return fmt.Errorf("Redis target %d: %w", target.ID, err)
	}
	if target.AlertEnabled && c.alerts != nil && sample.MaxMemory > 0 {
		for _, threshold := range []int64{80, 90, 100} {
			if memoryPercent(previous.UsedMemory, previous.MaxMemory) < threshold && memoryPercent(sample.UsedMemory, sample.MaxMemory) >= threshold {
				pct := memoryPercent(sample.UsedMemory, sample.MaxMemory)
				if alertErr := c.alerts.RedisMemoryAlert(ctx, target.Name, sample.UsedMemory, sample.MaxMemory, pct, threshold); alertErr != nil {
					return fmt.Errorf("queue Redis alert for target %d at %d%%: %w", target.ID, threshold, alertErr)
				}
			}
		}
	}
	return nil
}

func memoryPercent(used, maximum int64) int64 {
	if maximum <= 0 || used <= 0 {
		return 0
	}
	return int64(float64(used) * 100 / float64(maximum))
}

func safeError(err error) string {
	// Network and Redis errors may include hostnames or server-supplied data.
	// Keep only a short, single-line reason in the database and logs.
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 180 {
		s = s[:180]
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
		return store.RedisSample{}, errors.New("could not connect to Redis")
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
			return store.RedisSample{}, errors.New("Redis authentication failed")
		}
	}
	if db := strings.TrimPrefix(u.Path, "/"); db != "" && db != "0" {
		if n, err := strconv.Atoi(db); err != nil || n < 0 {
			return store.RedisSample{}, errors.New("Redis database path must be a number")
		}
		if _, err := command(r, w, "SELECT", db); err != nil {
			return store.RedisSample{}, errors.New("Redis database selection failed")
		}
	}
	info, err := command(r, w, "INFO")
	if err != nil {
		return store.RedisSample{}, err
	}
	return parseInfo(info)
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
		return "", errors.New("invalid Redis response")
	}
	switch line[0] {
	case '+':
		return strings.TrimSuffix(strings.TrimSuffix(line[1:], "\n"), "\r"), nil
	case '-':
		return "", errors.New("Redis returned an error")
	case '$':
		n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
		if err != nil || n < 0 || n > readLimit {
			return "", errors.New("Redis INFO response exceeds limit")
		}
		b := make([]byte, n+2)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		if b[n] != '\r' || b[n+1] != '\n' {
			return "", errors.New("invalid Redis bulk response")
		}
		return string(b[:n]), nil
	default:
		return "", errors.New("unexpected Redis response type")
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
			return 0, fmt.Errorf("Redis INFO is missing %s", key)
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("Redis INFO has invalid %s", key)
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
				return s, errors.New("Redis INFO has invalid key count")
			}
			s.KeyCount += n
		}
	}
	return s, nil
}
