// Package notify raises alerts from stored events and delivers them by email
// and webhook (PRD §4.7, M6).
//
// It is built so that notifications can never slow down ingestion:
//
//   - The ingest processor hands each stored batch to Handle, which only
//     copies it into a bounded buffer and returns.
//   - One evaluator goroutine decides which channels should hear about each
//     event, applies per-channel cooldowns, and records an alert as a
//     pending row in notification_history.
//   - Worker goroutines deliver alerts and retry failures with exponential
//     backoff. The history table is the durable queue: a sweeper picks up
//     anything due, so alerts survive a full in-memory queue and restarts.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// Defaults (D14: a 500-job notification queue, separate from ingestion).
const (
	DefaultQueueSize     = 500
	DefaultWorkers       = 2
	DefaultSweepInterval = 15 * time.Second
	channelCacheTTL      = 15 * time.Second
	resultBuffer         = 256 // ingest batches waiting for evaluation
)

// DefaultBackoff is the wait before each retry: a delivery is tried up to
// len(DefaultBackoff)+1 times over about 2h45m before it is marked failed.
var DefaultBackoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute, 2 * time.Hour}

// Options configures a Notifier.
type Options struct {
	Store         *store.Store
	Logger        *slog.Logger
	Key           []byte // auth.Keys.Encryption, to unseal channel secrets
	PublicURL     string // dashboard base URL for links in alerts
	Version       string
	Mailer        Mailer       // nil: real SMTP
	HTTPClient    *http.Client // nil: 10 s timeout, no redirects
	QueueSize     int
	Workers       int
	SweepInterval time.Duration
	Backoff       []time.Duration
	Now           func() time.Time
}

// Snapshot is the notifier's counters since start.
type Snapshot struct {
	Raised     int64 // alerts recorded
	Suppressed int64 // alerts skipped by a cooldown
	Sent       int64
	Failed     int64 // deliveries that used up their attempts
	Retried    int64 // failed attempts that will be retried
	Dropped    int64 // ingest batches dropped because the evaluator fell behind
}

// Notifier evaluates triggers and delivers alerts.
type Notifier struct {
	opts   Options
	store  *store.Store
	logger *slog.Logger
	now    func() time.Time

	results    chan []store.IngestResult
	deliveries chan int64

	mu       sync.Mutex
	inflight map[int64]bool
	// pending counts batches accepted by Handle and not yet evaluated, so
	// Flush can tell when the evaluator is truly idle.
	pending atomic.Int64

	// Owned by the evaluator goroutine.
	lastAlert    map[store.AlertKey]time.Time
	channels     []store.Channel
	projects     map[int64]string
	channelsAt   time.Time
	invalidated  atomic.Bool
	raised       atomic.Int64
	suppressed   atomic.Int64
	sent         atomic.Int64
	failed       atomic.Int64
	retried      atomic.Int64
	dropped      atomic.Int64
	ctx          context.Context
	cancel       context.CancelFunc
	evalDone     chan struct{}
	workersDone  sync.WaitGroup
	closeResults sync.Once
}

// New builds a notifier; call Start to run it.
func New(opts Options) *Notifier {
	if opts.QueueSize <= 0 {
		opts.QueueSize = DefaultQueueSize
	}
	if opts.Workers <= 0 {
		opts.Workers = DefaultWorkers
	}
	if opts.SweepInterval <= 0 {
		opts.SweepInterval = DefaultSweepInterval
	}
	if len(opts.Backoff) == 0 {
		opts.Backoff = DefaultBackoff
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Mailer == nil {
		opts.Mailer = SMTPMailer{}
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{
			Timeout: 10 * time.Second,
			// A redirect could point the signed request somewhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Notifier{
		opts: opts, store: opts.Store, logger: opts.Logger, now: opts.Now,
		results:    make(chan []store.IngestResult, resultBuffer),
		deliveries: make(chan int64, opts.QueueSize),
		inflight:   map[int64]bool{},
		lastAlert:  map[store.AlertKey]time.Time{},
		evalDone:   make(chan struct{}),
	}
}

// Start seeds the cooldowns from recent history and starts the goroutines.
func (n *Notifier) Start() error {
	n.ctx, n.cancel = context.WithCancel(context.Background())
	recent, err := n.store.RecentAlerts(n.ctx, n.now().Add(-store.MaxCooldownMinutes*time.Minute))
	if err != nil {
		return fmt.Errorf("load recent alerts: %w", err)
	}
	n.lastAlert = recent
	go n.evaluate()
	for range n.opts.Workers {
		n.workersDone.Add(1)
		go n.work()
	}
	go n.sweep()
	return nil
}

// Handle receives the results of one stored ingest batch. It never blocks:
// if the evaluator has fallen far behind, the oldest batch is dropped and
// counted.
func (n *Notifier) Handle(results []store.IngestResult) {
	batch := make([]store.IngestResult, 0, len(results))
	for _, r := range results {
		if !r.Duplicate && r.IssueID != 0 {
			batch = append(batch, r)
		}
	}
	if len(batch) == 0 {
		return
	}
	defer func() { _ = recover() }() // Handle after Shutdown: the channel is closed
	n.pending.Add(1)
	for {
		select {
		case n.results <- batch:
			return
		default:
		}
		select {
		case <-n.results:
			n.pending.Add(-1)
			n.dropped.Add(1)
		default:
		}
	}
}

// InvalidateChannels makes the evaluator reload channels before its next
// batch. The dashboard calls it after any channel change.
func (n *Notifier) InvalidateChannels() { n.invalidated.Store(true) }

// Stats returns the counters since start.
func (n *Notifier) Stats() Snapshot {
	return Snapshot{
		Raised: n.raised.Load(), Suppressed: n.suppressed.Load(), Sent: n.sent.Load(),
		Failed: n.failed.Load(), Retried: n.retried.Load(), Dropped: n.dropped.Load(),
	}
}

// Shutdown stops taking new results, evaluates what is buffered and lets
// in-flight deliveries finish, all within ctx. Undelivered alerts stay
// pending in the database and are sent after the next start.
func (n *Notifier) Shutdown(ctx context.Context) error {
	if n.cancel == nil {
		return nil
	}
	n.closeResults.Do(func() { close(n.results) })
	select {
	case <-n.evalDone:
	case <-ctx.Done():
	}
	n.cancel()
	done := make(chan struct{})
	go func() { n.workersDone.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Flush waits until every handed batch is evaluated and nothing is in
// flight or due. Tests use it instead of sleeping. With a short first
// backoff step (as in tests), it also waits for retries due within that
// step, so a just-failed attempt isn't mistaken for a finished one.
func (n *Notifier) Flush(ctx context.Context) error {
	var horizon time.Duration
	if first := n.opts.Backoff[0]; first <= time.Second {
		horizon = first
	}
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if n.pending.Load() == 0 && n.inflightCount() == 0 {
			due, err := n.store.DueNotifications(ctx, n.now().Add(horizon), 1)
			if err != nil {
				return err
			}
			if len(due) == 0 {
				return nil
			}
			n.enqueueDue(ctx)
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ---- evaluation ----

func (n *Notifier) evaluate() {
	defer close(n.evalDone)
	for batch := range n.results {
		n.evaluateBatch(batch)
		n.pending.Add(-1)
	}
}

func (n *Notifier) loadChannels(ctx context.Context) error {
	if !n.invalidated.Swap(false) && n.channels != nil && n.now().Sub(n.channelsAt) < channelCacheTTL {
		return nil
	}
	channels, err := n.store.Channels(ctx)
	if err != nil {
		return err
	}
	projects, err := n.store.Projects(ctx)
	if err != nil {
		return err
	}
	n.projects = make(map[int64]string, len(projects))
	for _, p := range projects {
		n.projects[p.ID] = p.Name
	}
	n.channels = n.channels[:0]
	for _, c := range channels {
		if c.Enabled {
			n.channels = append(n.channels, c)
		}
	}
	if n.channels == nil {
		n.channels = []store.Channel{}
	}
	n.channelsAt = n.now()
	return nil
}

// triggersFor lists the event triggers a stored event raises, before any
// channel's preferences.
func triggersFor(r store.IngestResult) []string {
	if r.Status == store.IssueIgnored {
		return nil
	}
	var out []string
	if r.Reopened {
		out = append(out, store.TriggerRegression)
	}
	if r.Category == store.CategoryQueue {
		// Retries that will run again don't alert; terminal and manual
		// failures do, whether the issue is new or not.
		if r.JobKind != "attempt" {
			out = append(out, store.TriggerQueueFailure)
		}
	} else if r.NewIssue {
		out = append(out, store.TriggerNewIssue)
	}
	return out
}

func wants(c store.Channel, trigger string) bool {
	switch trigger {
	case store.TriggerNewIssue:
		return c.OnNewIssue
	case store.TriggerRegression:
		return c.OnRegression
	case store.TriggerQueueFailure:
		return c.OnQueueFailure
	case store.TriggerThreshold:
		return c.ThresholdCount > 0
	}
	return false
}

func (n *Notifier) evaluateBatch(batch []store.IngestResult) {
	ctx := n.ctx
	if err := n.loadChannels(ctx); err != nil {
		n.logger.Error("load notification channels", "error", err)
		return
	}
	if len(n.channels) == 0 {
		return
	}
	thresholdChecked := map[store.AlertKey]bool{}
	for _, r := range batch {
		triggers := triggersFor(r)
		for _, c := range n.channels {
			if !c.Matches(r.ProjectID, r.Environment) {
				continue
			}
			for _, t := range triggers {
				if wants(c, t) {
					n.raise(ctx, c, r, t, 0)
				}
			}
			if c.ThresholdCount > 0 && r.Status != store.IssueIgnored {
				key := store.AlertKey{ChannelID: c.ID, IssueID: r.IssueID, Trigger: store.TriggerThreshold}
				if thresholdChecked[key] || n.cooling(c, key) {
					continue
				}
				thresholdChecked[key] = true
				window := time.Duration(c.ThresholdWindow) * time.Minute
				count, err := n.store.IssueEventsSince(ctx, r.IssueID, n.now().Add(-window))
				if err != nil {
					n.logger.Error("count events for threshold", "issue_id", r.IssueID, "error", err)
					continue
				}
				if count >= int64(c.ThresholdCount) {
					n.raise(ctx, c, r, store.TriggerThreshold, count)
				}
			}
		}
	}
}

// cooling reports whether the channel alerted for this issue and trigger
// within its cooldown.
func (n *Notifier) cooling(c store.Channel, key store.AlertKey) bool {
	last, ok := n.lastAlert[key]
	return ok && n.now().Sub(last) < time.Duration(c.Cooldown)*time.Minute
}

func (n *Notifier) raise(ctx context.Context, c store.Channel, r store.IngestResult, trigger string, count int64) {
	key := store.AlertKey{ChannelID: c.ID, IssueID: r.IssueID, Trigger: trigger}
	if n.cooling(c, key) {
		n.suppressed.Add(1)
		return
	}
	is, err := n.store.Issue(ctx, r.IssueID)
	if err != nil {
		n.logger.Error("load issue for alert", "issue_id", r.IssueID, "error", err)
		return
	}
	msg := buildMessage(trigger, c, is, n.projects[r.ProjectID], r.Environment, count, n.opts.PublicURL)
	payload, err := msg.encode()
	if err != nil {
		n.logger.Error("encode alert", "error", err)
		return
	}
	id, err := n.store.InsertNotification(ctx, store.Notification{
		ChannelID: c.ID, IssueID: is.ID, ProjectID: r.ProjectID, Trigger: trigger,
		Subject: msg.Subject, Payload: payload,
	})
	if err != nil {
		n.logger.Error("record alert", "channel_id", c.ID, "error", err)
		return
	}
	n.lastAlert[key] = n.now()
	n.raised.Add(1)
	n.enqueue(id)
}

// SendTest records a test alert for a channel and queues it for delivery.
func (n *Notifier) SendTest(ctx context.Context, channelID int64) (int64, error) {
	c, err := n.store.Channel(ctx, channelID)
	if err != nil {
		return 0, err
	}
	msg := testMessage(c)
	payload, err := msg.encode()
	if err != nil {
		return 0, err
	}
	id, err := n.store.InsertNotification(ctx, store.Notification{
		ChannelID: c.ID, Trigger: store.TriggerTest, Subject: msg.Subject, Payload: payload,
	})
	if err != nil {
		return 0, err
	}
	n.enqueue(id)
	return id, nil
}

// Retry makes a failed delivery pending again and queues it.
func (n *Notifier) Retry(ctx context.Context, id int64) error {
	if err := n.store.RequeueNotification(ctx, id); err != nil {
		return err
	}
	n.enqueue(id)
	return nil
}

// ---- delivery ----

// enqueue offers an alert to the workers. When the queue is full the alert
// simply stays pending in the database, and the sweeper delivers it later.
func (n *Notifier) enqueue(id int64) {
	n.mu.Lock()
	if n.inflight[id] {
		n.mu.Unlock()
		return
	}
	n.inflight[id] = true
	n.mu.Unlock()
	select {
	case n.deliveries <- id:
	default:
		n.release(id)
	}
}

func (n *Notifier) release(id int64) {
	n.mu.Lock()
	delete(n.inflight, id)
	n.mu.Unlock()
}

func (n *Notifier) inflightCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.inflight)
}

func (n *Notifier) sweep() {
	t := time.NewTicker(n.opts.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-t.C:
			n.enqueueDue(n.ctx)
		}
	}
}

func (n *Notifier) enqueueDue(ctx context.Context) {
	ids, err := n.store.DueNotifications(ctx, n.now(), n.opts.QueueSize)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			n.logger.Error("load due notifications", "error", err)
		}
		return
	}
	for _, id := range ids {
		n.enqueue(id)
	}
}

func (n *Notifier) work() {
	defer n.workersDone.Done()
	for {
		select {
		case <-n.ctx.Done():
			return
		case id := <-n.deliveries:
			n.deliver(n.ctx, id)
			n.release(id)
		}
	}
}

func (n *Notifier) deliver(ctx context.Context, id int64) {
	row, err := n.store.Notification(ctx, id)
	if err != nil || row.Status != store.NotificationPending {
		return // deleted with its channel, or already handled
	}
	c, err := n.store.Channel(ctx, row.ChannelID)
	if err != nil {
		return
	}
	attempts := row.Attempts + 1
	if !c.Enabled && row.Trigger != store.TriggerTest {
		n.markFailed(ctx, row, attempts, "the channel was turned off before delivery")
		return
	}
	var secret []byte
	if c.HasSecret() {
		if secret, err = auth.Unseal(n.opts.Key, c.Secret); err != nil {
			n.markFailed(ctx, row, attempts, err.Error())
			return
		}
	}
	msg, err := decodeMessage(row.Payload)
	if err != nil {
		n.markFailed(ctx, row, attempts, "stored message is unreadable: "+err.Error())
		return
	}
	msg.DeliveryID, msg.SentAt = row.ID, n.now().UTC()

	switch c.Kind {
	case store.ChannelWebhook:
		err = n.sendWebhook(ctx, c, secret, msg)
	case store.ChannelEmail:
		err = n.sendEmail(ctx, c, string(secret), msg)
	default:
		err = permanent(fmt.Errorf("unknown channel type %q", c.Kind))
	}
	if err == nil {
		if err := n.store.MarkNotificationSent(ctx, row.ID, attempts); err != nil {
			n.logger.Error("record delivery", "notification_id", row.ID, "error", err)
		}
		n.sent.Add(1)
		return
	}
	if ctx.Err() != nil {
		return // shutting down: leave it pending for the next start
	}
	var perm *permanentError
	if errors.As(err, &perm) || attempts > len(n.opts.Backoff) {
		n.markFailed(ctx, row, attempts, err.Error())
		return
	}
	next := n.now().Add(n.opts.Backoff[attempts-1])
	if err := n.store.MarkNotificationRetry(ctx, row.ID, attempts, next, err.Error()); err != nil {
		n.logger.Error("record delivery retry", "notification_id", row.ID, "error", err)
	}
	n.retried.Add(1)
	n.logger.Warn("notification delivery failed; will retry", "notification_id", row.ID, "channel_id", c.ID,
		"attempt", attempts, "next_attempt_at", next, "error", err)
}

func (n *Notifier) markFailed(ctx context.Context, row store.Notification, attempts int, msg string) {
	if err := n.store.MarkNotificationFailed(ctx, row.ID, attempts, msg); err != nil {
		n.logger.Error("record delivery failure", "notification_id", row.ID, "error", err)
	}
	n.failed.Add(1)
	n.logger.Error("notification delivery failed", "notification_id", row.ID, "channel_id", row.ChannelID,
		"attempts", attempts, "error", msg)
}

// permanentError marks a failure that retrying won't fix, such as a 4xx
// response or a rejected recipient.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

func permanent(err error) error { return &permanentError{err: err} }
