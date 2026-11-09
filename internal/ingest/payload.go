// Package ingest accepts events from the API, queues them in bounded
// memory, and writes them to the database in batches (PRD §4.2, D14).
package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sylvesteragyiri/faultbox/internal/grouping"
)

// Event kinds, which are also the issue categories.
const (
	KindException    = "exception"
	KindMessage      = "message"
	KindQueueFailure = "queue_failure"
)

// Queue failure kinds (PRD §4.5): a terminal failure after the last retry,
// a job failed manually with $this->fail(), or an intermediate attempt
// (optional reporting).
const (
	JobTerminal = "terminal"
	JobManual   = "manual"
	JobAttempt  = "attempt"
)

// Limits (D14). Text over these lengths is truncated; structured data over
// MaxObjectBytes is rejected.
const (
	MaxFrames        = 200
	MaxMessageLen    = 8 << 10
	MaxObjectBytes   = 32 << 10
	MaxURLLen        = 2048
	MaxShortField    = 200
	MaxTypeLen       = 512
	MaxFileLen       = 1024
	MaxPastSkew      = 24 * time.Hour
	MaxFutureSkew    = 5 * time.Minute
	keepFramesAtEach = MaxFrames / 2
)

// Levels follow PSR-3, which Laravel's logger uses.
var levels = []string{"emergency", "alert", "critical", "error", "warning", "notice", "info", "debug"}

// Payload is the JSON body of POST /api/v1/events and /api/v1/queue-failures.
// See docs/api/README.md. Unknown fields are ignored so newer SDKs keep working.
type Payload struct {
	EventID       string          `json:"event_id"`
	Timestamp     string          `json:"timestamp"`
	Level         string          `json:"level"`
	Environment   string          `json:"environment"`
	Release       string          `json:"release"`
	Exception     *Exception      `json:"exception"`
	Message       string          `json:"message"`
	Request       *Request        `json:"request"`
	User          json.RawMessage `json:"user"`
	Context       json.RawMessage `json:"context"`
	CorrelationID string          `json:"correlation_id"`
	Job           *Job            `json:"job"`
}

// Exception is the thrown exception. Frames are most recent call first,
// with frames[0] at the throw site.
type Exception struct {
	Type          string           `json:"type"`
	Message       string           `json:"message"`
	Frames        []grouping.Frame `json:"frames"`
	FramesOmitted int              `json:"frames_omitted"`
}

// Request describes the HTTP request being handled, if any.
type Request struct {
	URL        string            `json:"url,omitempty"`
	Method     string            `json:"method,omitempty"`
	StatusCode *int              `json:"status_code,omitempty"`
	Route      string            `json:"route,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
}

// Job describes a failed queue job.
type Job struct {
	Name       string          `json:"name"`
	ID         string          `json:"id,omitempty"`
	Queue      string          `json:"queue,omitempty"`
	Connection string          `json:"connection,omitempty"`
	Attempts   *int            `json:"attempts,omitempty"`
	MaxTries   *int            `json:"max_tries,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	BatchID    string          `json:"batch_id,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// Event is a validated payload, ready to queue.
type Event struct {
	ProjectID     int64
	Kind          string
	UUID          string
	OccurredAt    time.Time
	Level         string
	Environment   string
	Release       string
	ExceptionType string
	Message       string
	Frames        []grouping.Frame
	FramesOmitted int
	Request       *Request
	User          json.RawMessage
	Context       json.RawMessage
	CorrelationID string
	Job           *Job
	// Size estimates the event's memory footprint, for the queue's byte budget.
	Size int64
}

// FieldError is one validation problem, returned to the client in 422 responses.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError lists every problem found in a payload.
type ValidationError struct{ Fields []FieldError }

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		parts[i] = f.Field + ": " + f.Message
	}
	return "invalid event: " + strings.Join(parts, "; ")
}

// Decode parses a JSON payload. It rejects trailing data after the object.
func Decode(r io.Reader) (*Payload, error) {
	dec := json.NewDecoder(r)
	var p Payload
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("body is not a valid JSON event: %w", err)
	}
	if dec.More() {
		return nil, errors.New("body must contain exactly one JSON object")
	}
	return &p, nil
}

var (
	uuidRe   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	envRe    = regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)
	methodRe = regexp.MustCompile(`^[A-Z]{1,16}$`)
)

// Validate checks a payload for the given kind (KindException for
// /events, KindQueueFailure for /queue-failures) and returns the Event to
// queue. allowedEnvs is the project's environment list; empty allows any.
func Validate(p *Payload, kind string, projectID int64, allowedEnvs []string, now time.Time, bodySize int) (*Event, error) {
	var errs []FieldError
	fail := func(field, format string, args ...any) {
		errs = append(errs, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
	}

	ev := &Event{ProjectID: projectID, Kind: kind, Size: int64(bodySize)*2 + 512}

	switch {
	case p.EventID == "":
		fail("event_id", "is required")
	case !uuidRe.MatchString(p.EventID):
		fail("event_id", "must be a UUID")
	default:
		ev.UUID = strings.ToLower(p.EventID)
	}

	if p.Timestamp == "" {
		fail("timestamp", "is required")
	} else if ts, err := time.Parse(time.RFC3339Nano, p.Timestamp); err != nil {
		fail("timestamp", "must be an RFC 3339 date-time such as 2026-11-09T10:00:00Z")
	} else {
		switch {
		case ts.Before(now.Add(-MaxPastSkew)):
			fail("timestamp", "is more than 24 hours in the past")
		case ts.After(now.Add(MaxFutureSkew)):
			fail("timestamp", "is more than 5 minutes in the future; check the server clock")
		default:
			ev.OccurredAt = ts.UTC()
		}
	}

	ev.Level = strings.ToLower(strings.TrimSpace(p.Level))
	if ev.Level == "" {
		ev.Level = "error"
	} else if !slices.Contains(levels, ev.Level) {
		fail("level", "must be one of %s", strings.Join(levels, ", "))
	}

	ev.Environment = strings.ToLower(strings.TrimSpace(p.Environment))
	switch {
	case ev.Environment == "":
		fail("environment", "is required")
	case !envRe.MatchString(ev.Environment):
		fail("environment", "may only contain letters, digits, '.', '_' and '-' (max 64)")
	case len(allowedEnvs) > 0 && !slices.Contains(allowedEnvs, ev.Environment):
		fail("environment", "%q is not configured for this project (allowed: %s)", ev.Environment, strings.Join(allowedEnvs, ", "))
	}

	ev.Release = truncate(strings.TrimSpace(p.Release), MaxShortField)
	ev.CorrelationID = truncate(strings.TrimSpace(p.CorrelationID), MaxShortField)

	switch {
	case p.Exception != nil:
		ex := p.Exception
		ev.ExceptionType = strings.TrimSpace(ex.Type)
		if ev.ExceptionType == "" {
			fail("exception.type", "is required")
		} else if len(ev.ExceptionType) > MaxTypeLen {
			fail("exception.type", "must be at most %d characters", MaxTypeLen)
		}
		ev.Message = truncate(ex.Message, MaxMessageLen)
		ev.Frames, ev.FramesOmitted = limitFrames(ex.Frames)
		ev.FramesOmitted += max(ex.FramesOmitted, 0)
		for i := range ev.Frames {
			f := &ev.Frames[i]
			f.File = truncate(f.File, MaxFileLen)
			f.Function = truncate(f.Function, MaxTypeLen)
			f.Class = truncate(f.Class, MaxTypeLen)
			if f.Line < 0 {
				fail(fmt.Sprintf("exception.frames[%d].line", i), "must not be negative")
			}
		}
	case kind == KindException && strings.TrimSpace(p.Message) != "":
		ev.Kind = KindMessage
		ev.ExceptionType = "message"
		ev.Message = truncate(strings.TrimSpace(p.Message), MaxMessageLen)
	case kind == KindQueueFailure:
		fail("exception", "is required for queue failures")
	default:
		fail("exception", "is required (or send \"message\" for a plain message event)")
	}

	if r := p.Request; r != nil {
		r.URL = truncate(strings.TrimSpace(r.URL), MaxURLLen)
		r.Method = strings.ToUpper(strings.TrimSpace(r.Method))
		if r.Method != "" && !methodRe.MatchString(r.Method) {
			fail("request.method", "must be an HTTP method such as GET or POST")
		}
		if r.StatusCode != nil && (*r.StatusCode < 100 || *r.StatusCode > 599) {
			fail("request.status_code", "must be between 100 and 599")
		}
		r.Route = truncate(r.Route, MaxShortField)
		if headersSize(r.Headers) > MaxObjectBytes {
			fail("request.headers", "must be at most %d KB", MaxObjectBytes>>10)
		}
		ev.Request = r
	}

	ev.User = checkObject(p.User, "user", fail)
	ev.Context = checkObject(p.Context, "context", fail)

	if kind == KindQueueFailure {
		j := p.Job
		if j == nil {
			fail("job", "is required for queue failures")
		} else {
			j.Name = strings.TrimSpace(j.Name)
			if j.Name == "" {
				fail("job.name", "is required")
			} else if len(j.Name) > MaxTypeLen {
				fail("job.name", "must be at most %d characters", MaxTypeLen)
			}
			if j.Kind == "" {
				j.Kind = JobTerminal
			} else if j.Kind != JobTerminal && j.Kind != JobManual && j.Kind != JobAttempt {
				fail("job.kind", "must be terminal, manual or attempt")
			}
			if j.Attempts != nil && *j.Attempts < 0 {
				fail("job.attempts", "must not be negative")
			}
			if j.MaxTries != nil && *j.MaxTries < 0 {
				fail("job.max_tries", "must not be negative")
			}
			j.ID = truncate(j.ID, MaxShortField)
			j.Queue = truncate(j.Queue, MaxShortField)
			j.Connection = truncate(j.Connection, MaxShortField)
			j.BatchID = truncate(j.BatchID, MaxShortField)
			j.Payload = checkObject(j.Payload, "job.payload", fail)
			ev.Job = j
		}
	}

	if len(errs) > 0 {
		return nil, &ValidationError{Fields: errs}
	}
	return ev, nil
}

// limitFrames keeps the first and last 100 frames of very deep traces (D14).
func limitFrames(frames []grouping.Frame) ([]grouping.Frame, int) {
	if len(frames) <= MaxFrames {
		return frames, 0
	}
	kept := make([]grouping.Frame, 0, MaxFrames)
	kept = append(kept, frames[:keepFramesAtEach]...)
	kept = append(kept, frames[len(frames)-keepFramesAtEach:]...)
	return kept, len(frames) - MaxFrames
}

// checkObject accepts a JSON object up to MaxObjectBytes; null or absent
// means "not sent".
func checkObject(raw json.RawMessage, field string, fail func(string, string, ...any)) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] != '{' {
		fail(field, "must be a JSON object")
		return nil
	}
	if len(trimmed) > MaxObjectBytes {
		fail(field, "must be at most %d KB", MaxObjectBytes>>10)
		return nil
	}
	return trimmed
}

func headersSize(h map[string]string) int {
	n := 0
	for k, v := range h {
		n += len(k) + len(v) + 6
	}
	return n
}

// truncate shortens s to at most n bytes without splitting a UTF-8 character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
