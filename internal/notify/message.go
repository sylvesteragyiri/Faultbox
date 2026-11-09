package notify

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// Message is an alert as rendered when it was raised. It is stored as the
// history row's payload, sent as the webhook body, and turned into the
// email text.
type Message struct {
	DeliveryID  int64        `json:"delivery_id"` // set when sent
	Trigger     string       `json:"trigger"`
	Title       string       `json:"title"`
	Subject     string       `json:"subject"`
	Channel     string       `json:"channel"`
	Project     *ProjectRef  `json:"project,omitempty"`
	Environment string       `json:"environment,omitempty"`
	Issue       *IssueRef    `json:"issue,omitempty"`
	Threshold   *ThresholdOf `json:"threshold,omitempty"`
	SentAt      time.Time    `json:"sent_at"` // set when sent
}

// ProjectRef names the project an alert is about.
type ProjectRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// IssueRef summarises the issue an alert is about.
type IssueRef struct {
	ID              int64     `json:"id"`
	Title           string    `json:"title"`
	Category        string    `json:"category"`
	ExceptionType   string    `json:"exception_type"`
	Message         string    `json:"message"`
	Culprit         string    `json:"culprit,omitempty"`
	JobName         string    `json:"job_name,omitempty"`
	Status          string    `json:"status"`
	OccurrenceCount int64     `json:"occurrence_count"`
	FirstSeenAt     time.Time `json:"first_seen_at"`
	LastSeenAt      time.Time `json:"last_seen_at"`
	URL             string    `json:"url,omitempty"`
}

// ThresholdOf explains a threshold alert.
type ThresholdOf struct {
	Count         int64 `json:"count"`
	Limit         int   `json:"limit"`
	WindowMinutes int   `json:"window_minutes"`
}

func (m Message) encode() (string, error) {
	b, err := json.Marshal(m)
	return string(b), err
}

func decodeMessage(payload string) (Message, error) {
	var m Message
	err := json.Unmarshal([]byte(payload), &m)
	return m, err
}

// oneLine keeps user-controlled text (exception messages, job names) from
// breaking email headers or flooding subjects.
func oneLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > limit {
		return string(r[:limit-1]) + "…"
	}
	return s
}

func issueURL(publicURL string, id int64) string {
	if publicURL == "" {
		return ""
	}
	return strings.TrimRight(publicURL, "/") + "/issues/" + strconv.FormatInt(id, 10)
}

func windowText(minutes int) string {
	switch {
	case minutes%1440 == 0:
		return plural(minutes/1440, "day")
	case minutes%60 == 0:
		return plural(minutes/60, "hour")
	}
	return plural(minutes, "minute")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

func buildMessage(trigger string, c store.Channel, is store.Issue, project, env string, count int64, publicURL string) Message {
	title := is.ExceptionType
	if is.JobName != "" {
		title = is.JobName
	}
	title = oneLine(title, 120)
	m := Message{
		Trigger: trigger, Title: title, Channel: c.Name, Environment: env,
		Project: &ProjectRef{ID: is.ProjectID, Name: project},
		Issue: &IssueRef{
			ID: is.ID, Title: title, Category: is.Category, ExceptionType: is.ExceptionType,
			Message: oneLine(is.Message, 500), Culprit: is.Culprit, JobName: is.JobName, Status: is.Status,
			OccurrenceCount: is.OccurrenceCount, FirstSeenAt: is.FirstSeenAt.UTC(), LastSeenAt: is.LastSeenAt.UTC(),
			URL: issueURL(publicURL, is.ID),
		},
	}
	where := project
	if env != "" {
		where += " (" + env + ")"
	}
	switch trigger {
	case store.TriggerNewIssue:
		m.Subject = "New issue in " + where + ": " + title
	case store.TriggerRegression:
		m.Subject = "Regression in " + where + ": " + title
	case store.TriggerQueueFailure:
		m.Subject = "Job failed in " + where + ": " + title
	case store.TriggerThreshold:
		m.Threshold = &ThresholdOf{Count: count, Limit: c.ThresholdCount, WindowMinutes: c.ThresholdWindow}
		m.Subject = fmt.Sprintf("%s happened %d times in %s in %s", title, count, windowText(c.ThresholdWindow), where)
	}
	m.Subject = oneLine("[Faultbox] "+m.Subject, 200)
	return m
}

func testMessage(c store.Channel) Message {
	return Message{
		Trigger: store.TriggerTest, Title: "Test notification", Channel: c.Name,
		Subject: oneLine("[Faultbox] Test notification for "+c.Name, 200),
	}
}

var triggerHeadline = map[string]string{
	store.TriggerNewIssue:     "A new issue was detected",
	store.TriggerRegression:   "A resolved issue happened again",
	store.TriggerQueueFailure: "A queued job failed and won't be retried",
	store.TriggerThreshold:    "An issue passed its occurrence threshold",
	store.TriggerTest:         "This is a test notification",
}

// text renders the plain-text email body.
func (m Message) text() string {
	var b strings.Builder
	b.WriteString(triggerHeadline[m.Trigger])
	if m.Project != nil && m.Project.Name != "" {
		b.WriteString(" in " + m.Project.Name)
		if m.Environment != "" {
			b.WriteString(" (" + m.Environment + ")")
		}
	}
	b.WriteString(".\n\n")
	if is := m.Issue; is != nil {
		b.WriteString(is.Title + "\n")
		if is.JobName != "" {
			b.WriteString(is.ExceptionType + "\n")
		}
		b.WriteString(is.Message + "\n\n")
		if m.Threshold != nil {
			fmt.Fprintf(&b, "Occurrences: %d in the last %s (threshold %d)\n",
				m.Threshold.Count, windowText(m.Threshold.WindowMinutes), m.Threshold.Limit)
		}
		if is.Culprit != "" {
			b.WriteString("Where:       " + is.Culprit + "\n")
		}
		fmt.Fprintf(&b, "Events:      %d\n", is.OccurrenceCount)
		b.WriteString("First seen:  " + is.FirstSeenAt.Format("2006-01-02 15:04 UTC") + "\n")
		b.WriteString("Last seen:   " + is.LastSeenAt.Format("2006-01-02 15:04 UTC") + "\n")
		b.WriteString("Status:      " + is.Status + "\n")
		if is.URL != "" {
			b.WriteString("\nOpen in Faultbox: " + is.URL + "\n")
		}
	} else {
		b.WriteString("If you can read this, the channel is set up correctly.\n")
	}
	b.WriteString("\n--\nSent by the \"" + m.Channel + "\" channel in Faultbox.\n")
	return b.String()
}
