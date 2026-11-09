package ingest

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// Limits for Laravel notification batches (POST /api/v1/notifications).
const (
	MaxNotificationsPerBatch = 500
	maxClassLen              = 255
	maxChannelLen            = 64
	maxNotificationError     = 1000
)

// NotificationBatch is the body of POST /api/v1/notifications: the Laravel
// notifications one request, job or command sent, or failed to send.
type NotificationBatch struct {
	// BatchID identifies the batch; the SDK reuses it when it retries, so
	// the server counts a batch once.
	BatchID       string             `json:"batch_id"`
	Environment   string             `json:"environment"`
	Release       string             `json:"release"`
	Notifications []NotificationItem `json:"notifications"`
}

// NotificationItem is one notification on one channel.
type NotificationItem struct {
	ID            string             `json:"id"`
	Timestamp     string             `json:"timestamp"`
	Notification  string             `json:"notification"`
	Channel       string             `json:"channel"`
	Notifiable    string             `json:"notifiable"`
	Status        string             `json:"status"` // sent or failed
	Error         *NotificationError `json:"error"`
	CorrelationID string             `json:"correlation_id"`
}

// NotificationError says why a notification failed.
type NotificationError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

var (
	classRe   = regexp.MustCompile(`^[A-Za-z_\\][A-Za-z0-9_\\]*$`)
	channelRe = regexp.MustCompile(`^[A-Za-z0-9_.:\\-]+$`)
)

// DecodeNotifications parses a batch, rejecting unknown top-level shapes.
func DecodeNotifications(r io.Reader) (*NotificationBatch, error) {
	var b NotificationBatch
	dec := json.NewDecoder(r)
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	return &b, nil
}

// ValidateNotifications checks a batch and turns it into rows to store.
// allowedEnvs is the project's environment list; empty allows any.
func ValidateNotifications(b *NotificationBatch, allowedEnvs []string, now time.Time) ([]store.NewAppNotification, error) {
	var errs []FieldError
	fail := func(field, format string, args ...any) {
		errs = append(errs, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
	}

	if !uuidRe.MatchString(b.BatchID) {
		fail("batch_id", "must be a UUID")
	}
	env := strings.ToLower(strings.TrimSpace(b.Environment))
	switch {
	case env == "":
		fail("environment", "is required")
	case !envRe.MatchString(env):
		fail("environment", "may only contain letters, digits, '.', '_' and '-' (max 64)")
	case len(allowedEnvs) > 0 && !slices.Contains(allowedEnvs, env):
		fail("environment", "%q is not configured for this project (allowed: %s)", env, strings.Join(allowedEnvs, ", "))
	}
	release := truncate(strings.TrimSpace(b.Release), 200)
	switch n := len(b.Notifications); {
	case n == 0:
		fail("notifications", "must contain at least one notification")
	case n > MaxNotificationsPerBatch:
		fail("notifications", "may contain at most %d notifications", MaxNotificationsPerBatch)
	}

	out := make([]store.NewAppNotification, 0, len(b.Notifications))
	for i, it := range b.Notifications {
		if len(errs) >= 20 {
			break // enough to fix the SDK; don't build a huge error body
		}
		at := func(field string) string { return fmt.Sprintf("notifications[%d].%s", i, field) }
		n := store.NewAppNotification{Environment: env, Release: release}
		if !uuidRe.MatchString(it.ID) {
			fail(at("id"), "must be a UUID")
		}
		n.UUID = strings.ToLower(it.ID)
		ts, err := time.Parse(time.RFC3339Nano, it.Timestamp)
		switch {
		case err != nil:
			fail(at("timestamp"), "must be an RFC 3339 date-time")
		case ts.Before(now.Add(-MaxPastSkew)):
			fail(at("timestamp"), "is more than 24 hours in the past")
		case ts.After(now.Add(MaxFutureSkew)):
			fail(at("timestamp"), "is more than 5 minutes in the future; check the server clock")
		}
		n.OccurredAt = ts.UTC()
		if it.Notification == "" || len(it.Notification) > maxClassLen || !classRe.MatchString(it.Notification) {
			fail(at("notification"), "must be a PHP class name")
		}
		n.Notification = strings.TrimPrefix(it.Notification, `\`)
		if it.Channel == "" || len(it.Channel) > maxChannelLen || !channelRe.MatchString(it.Channel) {
			fail(at("channel"), "must be a channel name such as mail or database (max %d characters)", maxChannelLen)
		}
		n.Channel = it.Channel
		switch {
		case it.Notifiable == "":
			n.Notifiable = "unknown"
		case len(it.Notifiable) > maxClassLen || !classRe.MatchString(it.Notifiable):
			fail(at("notifiable"), "must be a PHP class name")
		default:
			n.Notifiable = strings.TrimPrefix(it.Notifiable, `\`)
		}
		switch it.Status {
		case "sent":
		case "failed":
			n.Failed = true
			n.ErrorType, n.ErrorMessage = "UnknownError", "The notification channel reported a failure."
			if it.Error != nil {
				if t := strings.TrimSpace(it.Error.Type); t != "" {
					n.ErrorType = truncate(t, maxClassLen)
				}
				if m := strings.TrimSpace(it.Error.Message); m != "" {
					n.ErrorMessage = truncate(m, maxNotificationError)
				}
			}
		default:
			fail(at("status"), `must be "sent" or "failed"`)
		}
		n.CorrelationID = truncate(strings.TrimSpace(it.CorrelationID), 200)
		out = append(out, n)
	}
	if len(errs) > 0 {
		return nil, &ValidationError{Fields: errs}
	}
	return out, nil
}
