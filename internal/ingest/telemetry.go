package ingest

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// Limits for telemetry batches (POST /api/v1/telemetry).
const (
	MaxItemsPerList      = 500 // per list: notifications, mails, commands, cache
	maxClassLen          = 255
	maxChannelLen        = 64
	maxCommandLen        = 128
	maxNotificationError = 1000
	maxRecipients        = 10_000
	maxCommandDuration   = 7 * 24 * time.Hour
	maxCacheKeyLen       = 200
	maxCacheCount        = 1_000_000_000
	maxQueueNameLen      = 255
	maxJobIDLen          = 64
	maxJobAttempt        = 1_000_000
	maxJobWait           = 366 * 24 * time.Hour // delayed jobs wait as long as they were delayed
)

// TelemetryBatch is the body of POST /api/v1/telemetry (and of the older
// POST /api/v1/notifications, which only sends notifications): what one
// request, job or command did besides throwing.
type TelemetryBatch struct {
	// BatchID identifies the batch; the SDK reuses it when it retries, so
	// the server counts a batch once.
	BatchID       string             `json:"batch_id"`
	Environment   string             `json:"environment"`
	Release       string             `json:"release"`
	Notifications []NotificationItem `json:"notifications"`
	Mails         []MailItem         `json:"mails"`
	Commands      []CommandItem      `json:"commands"`
	Cache         []CacheItem        `json:"cache"`
	Jobs          []JobItem          `json:"jobs"`
}

// NotificationItem is one Laravel notification on one channel.
type NotificationItem struct {
	ID            string     `json:"id"`
	Timestamp     string     `json:"timestamp"`
	Notification  string     `json:"notification"`
	Channel       string     `json:"channel"`
	Notifiable    string     `json:"notifiable"`
	Status        string     `json:"status"` // sent or failed
	Error         *ItemError `json:"error"`
	CorrelationID string     `json:"correlation_id"`
}

// MailItem is one email sent directly (a Mailable or Mail::raw).
type MailItem struct {
	ID            string     `json:"id"`
	Timestamp     string     `json:"timestamp"`
	Mailable      string     `json:"mailable"` // class; empty for Mail::raw and closures
	Mailer        string     `json:"mailer"`
	Recipients    int        `json:"recipients"` // To + Cc + Bcc, a count only
	Status        string     `json:"status"`     // sent or failed
	Error         *ItemError `json:"error"`
	CorrelationID string     `json:"correlation_id"`
}

// CommandItem is one artisan command run.
type CommandItem struct {
	ID         string     `json:"id"`
	Timestamp  string     `json:"timestamp"` // when it started
	Command    string     `json:"command"`
	ExitCode   *int       `json:"exit_code"`
	DurationMs int64      `json:"duration_ms"`
	Error      *ItemError `json:"error"`
}

// CacheItem totals what the app did with one cache store and key pattern
// since its last flush.
type CacheItem struct {
	Timestamp string `json:"timestamp"` // when the totals were taken
	Store     string `json:"store"`
	Key       string `json:"key"` // a pattern such as users:*:profile, never a raw key with IDs
	Hits      int64  `json:"hits"`
	Misses    int64  `json:"misses"`
	Writes    int64  `json:"writes"`
	Deletes   int64  `json:"deletes"`
	Failures  int64  `json:"failures"` // failed writes and deletes
}

// JobItem is one attempt of one queued job, successful ones included.
// Never the payload, constructor arguments or tags: only what identifies
// the job class and how the attempt went.
type JobItem struct {
	ID            string `json:"id"`     // this attempt
	UUID          string `json:"uuid"`   // the job's payload UUID, shared by its attempts
	JobID         string `json:"job_id"` // the queue's job ID, as sent with a failed job
	Attempt       int    `json:"attempt"`
	MaxTries      *int   `json:"max_tries"`
	Job           string `json:"job"`  // displayName: the notification, mailable or listener, not the wrapper
	Kind          string `json:"kind"` // job, notification, mail, listener or broadcast
	Queue         string `json:"queue"`
	Connection    string `json:"connection"`
	Outcome       string `json:"outcome"` // succeeded, released, failed or timed_out
	DurationMs    *int64 `json:"duration_ms"`
	WaitMs        *int64 `json:"wait_ms"` // from dispatch to start; absent when unknown
	StartedAt     string `json:"started_at"`
	CorrelationID string `json:"correlation_id"`
	BatchID       string `json:"batch_id"` // Laravel job batch (Bus::batch)
}

// ItemError says why something failed.
type ItemError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

var (
	classRe   = regexp.MustCompile(`^[A-Za-z_\\][A-Za-z0-9_\\]*$`)
	channelRe = regexp.MustCompile(`^[A-Za-z0-9_.:\\-]+$`)
	commandRe = regexp.MustCompile(`^[A-Za-z0-9:_.-]+$`)
)

// DecodeTelemetry parses a batch.
func DecodeTelemetry(r io.Reader) (*TelemetryBatch, error) {
	var b TelemetryBatch
	if err := json.NewDecoder(r).Decode(&b); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	return &b, nil
}

// ValidateTelemetry checks a batch and turns it into rows to store.
// allowedEnvs is the project's environment list; empty allows any.
func ValidateTelemetry(b *TelemetryBatch, allowedEnvs []string, now time.Time) (store.TelemetryBatch, error) {
	v := telemetryValidator{now: now}

	if !uuidRe.MatchString(b.BatchID) {
		v.fail("batch_id", "must be a UUID")
	}
	env := strings.ToLower(strings.TrimSpace(b.Environment))
	switch {
	case env == "":
		v.fail("environment", "is required")
	case !envRe.MatchString(env):
		v.fail("environment", "may only contain letters, digits, '.', '_' and '-' (max 64)")
	case len(allowedEnvs) > 0 && !slices.Contains(allowedEnvs, env):
		v.fail("environment", "%q is not configured for this project (allowed: %s)", env, strings.Join(allowedEnvs, ", "))
	}
	release := truncate(strings.TrimSpace(b.Release), 200)
	if len(b.Notifications)+len(b.Mails)+len(b.Commands)+len(b.Cache)+len(b.Jobs) == 0 {
		v.fail("notifications", "the batch must contain at least one notification, mail, command, cache entry or job run")
	}
	for name, n := range map[string]int{"notifications": len(b.Notifications), "mails": len(b.Mails), "commands": len(b.Commands), "cache": len(b.Cache), "jobs": len(b.Jobs)} {
		if n > MaxItemsPerList {
			v.fail(name, "may contain at most %d items", MaxItemsPerList)
		}
	}

	out := store.TelemetryBatch{BatchID: strings.ToLower(b.BatchID)}
	for i, it := range b.Notifications {
		if v.full() {
			break
		}
		at := func(f string) string { return fmt.Sprintf("notifications[%d].%s", i, f) }
		n := store.NewAppNotification{Environment: env, Release: release}
		n.UUID = v.id(at("id"), it.ID)
		n.OccurredAt = v.timestamp(at("timestamp"), it.Timestamp)
		n.Notification = v.class(at("notification"), it.Notification, false)
		n.Channel = v.channel(at("channel"), it.Channel)
		n.Notifiable = v.class(at("notifiable"), it.Notifiable, true)
		n.Failed, n.ErrorType, n.ErrorMessage = v.status(at("status"), it.Status, it.Error, "The notification channel reported a failure.")
		n.CorrelationID = truncate(strings.TrimSpace(it.CorrelationID), 200)
		out.Notifications = append(out.Notifications, n)
	}
	for i, it := range b.Mails {
		if v.full() {
			break
		}
		at := func(f string) string { return fmt.Sprintf("mails[%d].%s", i, f) }
		m := store.NewAppMail{Environment: env, Release: release}
		m.UUID = v.id(at("id"), it.ID)
		m.OccurredAt = v.timestamp(at("timestamp"), it.Timestamp)
		m.Mailable = "raw"
		if it.Mailable != "" {
			m.Mailable = v.class(at("mailable"), it.Mailable, false)
		}
		m.Mailer = v.channel(at("mailer"), it.Mailer)
		if it.Recipients < 0 || it.Recipients > maxRecipients {
			v.fail(at("recipients"), "must be a count from 0 to %d", maxRecipients)
		}
		m.Recipients = it.Recipients
		m.Failed, m.ErrorType, m.ErrorMessage = v.status(at("status"), it.Status, it.Error, "The mailer reported a failure.")
		m.CorrelationID = truncate(strings.TrimSpace(it.CorrelationID), 200)
		out.Mails = append(out.Mails, m)
	}
	for i, it := range b.Commands {
		if v.full() {
			break
		}
		at := func(f string) string { return fmt.Sprintf("commands[%d].%s", i, f) }
		c := store.NewCommandRun{Environment: env, Release: release}
		c.UUID = v.id(at("id"), it.ID)
		c.StartedAt = v.timestamp(at("timestamp"), it.Timestamp)
		if it.Command == "" || len(it.Command) > maxCommandLen || !commandRe.MatchString(it.Command) {
			v.fail(at("command"), "must be an artisan command name such as migrate or app:sync-orders")
		}
		c.Command = it.Command
		switch {
		case it.ExitCode == nil:
			v.fail(at("exit_code"), "is required")
		case *it.ExitCode < -1 || *it.ExitCode > 255:
			v.fail(at("exit_code"), "must be from -1 to 255")
		default:
			c.ExitCode = *it.ExitCode
		}
		if it.DurationMs < 0 || it.DurationMs > maxCommandDuration.Milliseconds() {
			v.fail(at("duration_ms"), "must be from 0 to %d", maxCommandDuration.Milliseconds())
		}
		c.DurationMs = it.DurationMs
		if it.Error != nil {
			c.ErrorType = truncate(strings.TrimSpace(it.Error.Type), maxClassLen)
			c.ErrorMessage = truncate(strings.TrimSpace(it.Error.Message), maxNotificationError)
		}
		out.Commands = append(out.Commands, c)
	}
	for i, it := range b.Cache {
		if v.full() {
			break
		}
		at := func(f string) string { return fmt.Sprintf("cache[%d].%s", i, f) }
		c := store.NewCacheCount{Environment: env, Hits: it.Hits, Misses: it.Misses, Writes: it.Writes, Deletes: it.Deletes, Failures: it.Failures}
		c.At = v.timestamp(at("timestamp"), it.Timestamp)
		c.Store = v.channel(at("store"), it.Store)
		if it.Key == "" || len(it.Key) > maxCacheKeyLen || !utf8.ValidString(it.Key) || strings.IndexFunc(it.Key, unicode.IsControl) >= 0 {
			v.fail(at("key"), "must be a key pattern of 1 to %d printable characters", maxCacheKeyLen)
		}
		c.Key = it.Key
		for name, n := range map[string]int64{"hits": it.Hits, "misses": it.Misses, "writes": it.Writes, "deletes": it.Deletes, "failures": it.Failures} {
			if n < 0 || n > maxCacheCount {
				v.fail(at(name), "must be a count from 0 to %d", maxCacheCount)
			}
		}
		out.Cache = append(out.Cache, c)
	}
	for i, it := range b.Jobs {
		if v.full() {
			break
		}
		out.Jobs = append(out.Jobs, v.job(i, it, env, release))
	}
	if len(v.errs) > 0 {
		return store.TelemetryBatch{}, &ValidationError{Fields: v.errs}
	}
	return out, nil
}

// job validates one job run.
func (v *telemetryValidator) job(i int, it JobItem, env, release string) store.NewJobRun {
	at := func(f string) string { return fmt.Sprintf("jobs[%d].%s", i, f) }
	j := store.NewJobRun{Environment: env, Release: release}
	j.UUID = v.id(at("id"), it.ID)
	if it.UUID != "" {
		j.JobUUID = v.id(at("uuid"), it.UUID)
	}
	if it.BatchID != "" {
		j.BatchID = v.id(at("batch_id"), it.BatchID)
	}
	if it.JobID != "" && !printableToken(it.JobID, maxJobIDLen) {
		v.fail(at("job_id"), "must be 1 to %d printable characters without spaces", maxJobIDLen)
	}
	j.JobID = it.JobID
	if it.Attempt < 1 || it.Attempt > maxJobAttempt {
		v.fail(at("attempt"), "must be from 1 to %d", maxJobAttempt)
	}
	j.Attempt = it.Attempt
	if it.MaxTries != nil && (*it.MaxTries < 0 || *it.MaxTries > maxJobAttempt) {
		v.fail(at("max_tries"), "must be from 0 to %d", maxJobAttempt)
	}
	j.MaxTries = it.MaxTries
	// Only a class name (or Closure) is accepted, so free text such as an
	// email address in a custom displayName can't be stored (PRD §8).
	j.JobName = v.class(at("job"), it.Job, false)
	j.Kind = it.Kind
	if j.Kind == "" {
		j.Kind = "job"
	}
	if !slices.Contains(store.JobKinds, j.Kind) {
		v.fail(at("kind"), "must be one of %s", strings.Join(store.JobKinds, ", "))
	}
	for _, f := range []struct {
		name, val string
		dst       *string
	}{{"queue", it.Queue, &j.Queue}, {"connection", it.Connection, &j.Connection}} {
		if f.val != "" && !printableToken(f.val, maxQueueNameLen) {
			v.fail(at(f.name), "must be 1 to %d printable characters without spaces", maxQueueNameLen)
		}
		*f.dst = f.val
	}
	if !slices.Contains(store.JobOutcomes, it.Outcome) {
		v.fail(at("outcome"), "must be one of %s", strings.Join(store.JobOutcomes, ", "))
	}
	j.Outcome = it.Outcome
	switch {
	case it.DurationMs == nil:
		v.fail(at("duration_ms"), "is required")
	case *it.DurationMs < 0 || *it.DurationMs > maxCommandDuration.Milliseconds():
		v.fail(at("duration_ms"), "must be from 0 to %d", maxCommandDuration.Milliseconds())
	default:
		j.DurationMs = *it.DurationMs
	}
	if it.WaitMs != nil {
		if *it.WaitMs < 0 || *it.WaitMs > maxJobWait.Milliseconds() {
			v.fail(at("wait_ms"), "must be from 0 to %d", maxJobWait.Milliseconds())
		}
		j.WaitMs = it.WaitMs
	}
	j.StartedAt = v.timestamp(at("started_at"), it.StartedAt)
	j.CorrelationID = truncate(strings.TrimSpace(it.CorrelationID), 200)
	return j
}

// printableToken reports whether s is 1 to n bytes of printable UTF-8 with
// no whitespace: a queue or connection name, or a job ID.
func printableToken(s string, n int) bool {
	if s == "" || len(s) > n || !utf8.ValidString(s) {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) < 0
}

// telemetryValidator collects field errors, stopping at 20: enough to fix
// an SDK without building a huge error body.
type telemetryValidator struct {
	now  time.Time
	errs []FieldError
}

func (v *telemetryValidator) fail(field, format string, args ...any) {
	v.errs = append(v.errs, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
}

func (v *telemetryValidator) full() bool { return len(v.errs) >= 20 }

func (v *telemetryValidator) id(field, id string) string {
	if !uuidRe.MatchString(id) {
		v.fail(field, "must be a UUID")
	}
	return strings.ToLower(id)
}

func (v *telemetryValidator) timestamp(field, s string) time.Time {
	ts, err := time.Parse(time.RFC3339Nano, s)
	switch {
	case err != nil:
		v.fail(field, "must be an RFC 3339 date-time")
	case ts.Before(v.now.Add(-MaxPastSkew)):
		v.fail(field, "is more than 24 hours in the past")
	case ts.After(v.now.Add(MaxFutureSkew)):
		v.fail(field, "is more than 5 minutes in the future; check the server clock")
	}
	return ts.UTC()
}

// class checks a PHP class name. optional names default to "unknown".
func (v *telemetryValidator) class(field, name string, optional bool) string {
	if name == "" && optional {
		return "unknown"
	}
	if name == "" || len(name) > maxClassLen || !classRe.MatchString(name) {
		v.fail(field, "must be a PHP class name")
	}
	return strings.TrimPrefix(name, `\`)
}

func (v *telemetryValidator) channel(field, name string) string {
	if name == "" || len(name) > maxChannelLen || !channelRe.MatchString(name) {
		v.fail(field, "must be a name such as mail or smtp (max %d characters)", maxChannelLen)
	}
	return name
}

func (v *telemetryValidator) status(field, status string, e *ItemError, fallback string) (failed bool, errType, errMsg string) {
	switch status {
	case "sent":
		return false, "", ""
	case "failed":
		errType, errMsg = "UnknownError", fallback
		if e != nil {
			if t := strings.TrimSpace(e.Type); t != "" {
				errType = truncate(t, maxClassLen)
			}
			if m := strings.TrimSpace(e.Message); m != "" {
				errMsg = truncate(m, maxNotificationError)
			}
		}
		return true, errType, errMsg
	}
	v.fail(field, `must be "sent" or "failed"`)
	return false, "", ""
}
