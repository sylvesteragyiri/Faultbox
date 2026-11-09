package main

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/ingest"
	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// Telemetry demo data: the Laravel notifications, mail, artisan commands and
// cache use of the demo projects. One batch per project and hour, built the
// way the SDK builds them and stored through the real validation and ingest
// code (ValidateTelemetry, IngestTelemetry), each against its own backdated
// clock.

type notificationSpec struct {
	class, channel, notifiable string
	perHour                    float64
	failRate                   float64
	errType, errMsg            string
}

type mailSpec struct {
	mailable, mailer string // mailable "" is Mail::raw
	perHour          float64
	recipients       int
	failRate         float64
	errType, errMsg  string
}

type commandSpec struct {
	command         string
	everyHours      int     // the scheduler runs it every n hours; 0 is by hand, now and then
	durationMs      float64 // typical run time
	failRate        float64
	exitCode        int // when it fails without throwing
	errType, errMsg string
}

type cacheSpec struct {
	store, key   string
	readsPerHour float64
	hitRate      float64
	deletesShare float64 // deletes per read
	failRate     float64 // failed writes per write
}

// incident makes things fail for a few hours, a few days ago: an SMS
// gateway outage, a flaky SMTP relay and a Redis that drops writes.
type incident struct {
	startAgo, hours int     // hours ago it started, and how long it lasted
	failRate        float64 // replaces the failure rate of the affected specs
}

type projectTelemetry struct {
	notifications []notificationSpec
	mails         []mailSpec
	commands      []commandSpec
	cache         []cacheSpec
	incident      incident
	// which specs the incident hits, by name: a class, mailable, command or key
	hit map[string]bool
}

// telemetryFor follows the order of projects.
var telemetryFor = []projectTelemetry{
	{ // Shop API
		notifications: []notificationSpec{
			{class: `App\Notifications\OrderShipped`, channel: "mail", notifiable: `App\Models\User`, perHour: 14, failRate: 0.01,
				errType: `Symfony\Component\Mailer\Exception\TransportException`, errMsg: `Expected response code "250" but got code "451", with message "451 4.3.0 Temporary lookup failure"`},
			{class: `App\Notifications\OrderShipped`, channel: "database", notifiable: `App\Models\User`, perHour: 14},
			{class: `App\Notifications\OrderOtp`, channel: "hubtel_sms", notifiable: `Illuminate\Notifications\AnonymousNotifiable`, perHour: 9, failRate: 0.03,
				errType: `Illuminate\Http\Client\RequestException`, errMsg: `HTTP request returned status code 503: {"message":"Service temporarily unavailable"}`},
			{class: `App\Notifications\LowStock`, channel: "slack", notifiable: `Illuminate\Notifications\AnonymousNotifiable`, perHour: 0.6, failRate: 0.02,
				errType: `Illuminate\Http\Client\ConnectionException`, errMsg: `cURL error 28: Operation timed out after 5001 milliseconds`},
			{class: `App\Notifications\Auth\ResetPassword`, channel: "mail", notifiable: `App\Models\User`, perHour: 1.5},
		},
		mails: []mailSpec{
			{mailable: `App\Mail\OrderConfirmation`, mailer: "smtp", perHour: 16, recipients: 1, failRate: 0.01,
				errType: `Symfony\Component\Mailer\Exception\TransportException`, errMsg: `Connection could not be established with host "smtp.mailgun.org:587": stream_socket_client(): Unable to connect`},
			{mailable: `App\Mail\Auth\EmailOTP`, mailer: "smtp", perHour: 7, recipients: 1, failRate: 0.02,
				errType: `Symfony\Component\Mailer\Exception\TransportException`, errMsg: `Expected response code "250" but got code "550", with message "550 5.1.1 Mailbox unavailable"`},
			{mailable: `App\Mail\Admin\DailySalesReport`, mailer: "ses", perHour: 1.0 / 24, recipients: 4},
			{mailable: "", mailer: "log", perHour: 0.3, recipients: 1},
		},
		commands: []commandSpec{
			{command: "app:sync-inventory", everyHours: 1, durationMs: 4200, failRate: 0.04,
				errType: `Illuminate\Http\Client\ConnectionException`, errMsg: `cURL error 7: Failed to connect to erp.internal port 443: Connection refused`},
			{command: "app:send-abandoned-cart-reminders", everyHours: 6, durationMs: 1800},
			{command: "app:prune-carts", everyHours: 24, durationMs: 950},
			{command: "model:prune", everyHours: 24, durationMs: 3100},
			{command: "migrate", durationMs: 2600, failRate: 0.2,
				errType: `Illuminate\Database\QueryException`, errMsg: `SQLSTATE[42S01]: Base table or view already exists: 1050 Table 'coupons' already exists`},
			{command: "app:import-products", durationMs: 95_000, failRate: 0.3, exitCode: 2},
		},
		cache: []cacheSpec{
			{store: "redis", key: "products:*:detail", readsPerHour: 2400, hitRate: 0.94, deletesShare: 0.004},
			{store: "redis", key: "categories:tree", readsPerHour: 1800, hitRate: 0.99},
			{store: "redis", key: "cart:*", readsPerHour: 900, hitRate: 0.71, deletesShare: 0.05, failRate: 0.001},
			{store: "redis", key: "settings", readsPerHour: 3000, hitRate: 0.998},
			{store: "redis", key: "search:*:page:*", readsPerHour: 650, hitRate: 0.22},
			{store: "redis", key: "users:*:wishlist", readsPerHour: 240, hitRate: 0.83, deletesShare: 0.02},
			{store: "file", key: "sitemap", readsPerHour: 2, hitRate: 0.9},
		},
		incident: incident{startAgo: 80, hours: 5, failRate: 0.35},
		hit:      map[string]bool{`App\Notifications\OrderOtp`: true, `App\Mail\Auth\EmailOTP`: true, "cart:*": true},
	},
	{ // Billing Worker
		notifications: []notificationSpec{
			{class: `App\Notifications\InvoicePaid`, channel: "mail", notifiable: `App\Models\Customer`, perHour: 5},
			{class: `App\Notifications\PaymentFailed`, channel: "mail", notifiable: `App\Models\Customer`, perHour: 0.8, failRate: 0.02,
				errType: `Aws\Ses\Exception\SesException`, errMsg: `Throttling: Maximum sending rate exceeded.`},
			{class: `App\Notifications\PaymentFailed`, channel: "vonage", notifiable: `App\Models\Customer`, perHour: 0.8, failRate: 0.05,
				errType: `Vonage\Client\Exception\Request`, errMsg: `Non-Whitelisted Destination`},
		},
		mails: []mailSpec{
			{mailable: `App\Mail\InvoiceReceipt`, mailer: "ses", perHour: 5, recipients: 2, failRate: 0.005,
				errType: `Aws\Ses\Exception\SesException`, errMsg: `MessageRejected: Email address is not verified.`},
			{mailable: `App\Mail\DunningReminder`, mailer: "ses", perHour: 0.5, recipients: 1},
		},
		commands: []commandSpec{
			{command: "billing:charge-due", everyHours: 24, durationMs: 48_000, failRate: 0.05,
				errType: `Stripe\Exception\RateLimitException`, errMsg: `Request rate limit exceeded. Lower your request rate.`},
			{command: "billing:reconcile", everyHours: 1, durationMs: 7800, failRate: 0.02,
				errType: `Illuminate\Database\QueryException`, errMsg: `SQLSTATE[40001]: Serialization failure: 1213 Deadlock found when trying to get lock`},
			{command: "billing:sync-exchange-rates", everyHours: 6, durationMs: 1200},
			{command: "queue:prune-failed", everyHours: 24, durationMs: 300},
		},
		cache: []cacheSpec{
			{store: "redis", key: "customer:*:plan", readsPerHour: 520, hitRate: 0.9, deletesShare: 0.01},
			{store: "redis", key: "exchange-rates:*", readsPerHour: 180, hitRate: 0.58},
			{store: "redis", key: "stripe:prices", readsPerHour: 300, hitRate: 0.97},
		},
		incident: incident{startAgo: 30, hours: 3, failRate: 0.6},
		hit:      map[string]bool{"billing:reconcile": true},
	},
	{ // Marketing Site
		notifications: []notificationSpec{
			{class: `App\Notifications\ContactFormSubmitted`, channel: "slack", notifiable: `Illuminate\Notifications\AnonymousNotifiable`, perHour: 0.7},
			{class: `App\Notifications\ContactFormSubmitted`, channel: "mail", notifiable: `Illuminate\Notifications\AnonymousNotifiable`, perHour: 0.7},
		},
		mails: []mailSpec{
			{mailable: `App\Mail\NewsletterWelcome`, mailer: "mailgun", perHour: 2.5, recipients: 1, failRate: 0.03,
				errType: `Symfony\Component\Mailer\Exception\HttpTransportException`, errMsg: `Unable to send an email: Domain example.com is not allowed to send: Free accounts are for test purposes only. (code 403).`},
		},
		commands: []commandSpec{
			{command: "sitemap:generate", everyHours: 24, durationMs: 12_500},
			{command: "app:warm-cache", everyHours: 3, durationMs: 2100},
		},
		cache: []cacheSpec{
			{store: "file", key: "blog:posts:page:*", readsPerHour: 420, hitRate: 0.88},
			{store: "file", key: "blog:post:*", readsPerHour: 610, hitRate: 0.81},
			{store: "file", key: "pricing:table", readsPerHour: 150, hitRate: 0.99},
		},
	},
}

// seedTelemetry stores one batch per project and hour over the last `days`
// days. scale multiplies every rate.
func seedTelemetry(ctx context.Context, st *store.Store, rng *rand.Rand, days int, scale float64) (batches, items int, err error) {
	now := time.Now().UTC()
	period := time.Duration(days) * 24 * time.Hour
	first := now.Add(-period).Truncate(time.Hour)
	for pi, p := range projects {
		spec := telemetryFor[pi]
		for hour := first; !hour.After(now); hour = hour.Add(time.Hour) {
			b := spec.batch(rng, p, hour, now, period, scale)
			if len(b.Notifications)+len(b.Mails)+len(b.Commands)+len(b.Cache) == 0 {
				continue
			}
			valid, err := ingest.ValidateTelemetry(b, p.envs, hour.Add(time.Hour+time.Minute))
			if err != nil {
				return batches, items, fmt.Errorf("%s at %s: %w", p.name, hour.Format(time.RFC3339), err)
			}
			if _, err := st.IngestTelemetry(ctx, p.id, valid); err != nil {
				return batches, items, err
			}
			batches++
			items += len(valid.Notifications) + len(valid.Mails) + len(valid.Commands) + len(valid.Cache)
		}
		fmt.Fprintf(os.Stderr, "  telemetry for %s stored\n", p.name)
	}
	return batches, items, nil
}

func (s projectTelemetry) batch(rng *rand.Rand, p *project, hour, now time.Time, period time.Duration, scale float64) *ingest.TelemetryBatch {
	ago := int(now.Sub(hour).Hours())
	inIncident := s.incident.hours > 0 && ago <= s.incident.startAgo && ago > s.incident.startAgo-s.incident.hours
	failRate := func(name string, normal float64) float64 {
		if inIncident && s.hit[name] {
			return s.incident.failRate
		}
		return normal
	}
	load := diurnal(hour) * scale
	// Within the hour, never in the future.
	at := func() string {
		t := hour.Add(time.Duration(rng.Int64N(int64(time.Hour))))
		if t.After(now) {
			t = now.Add(-time.Duration(rng.Int64N(int64(time.Minute))))
		}
		return t.Format(time.RFC3339Nano)
	}
	env := "production"
	if len(p.envs) > 1 && rng.IntN(6) == 0 {
		env = p.envs[1]
	}
	b := &ingest.TelemetryBatch{BatchID: newUUID(), Environment: env, Release: releaseAt(hour, now, period)}

	for _, n := range s.notifications {
		for range poisson(rng, n.perHour*load) {
			it := ingest.NotificationItem{ID: newUUID(), Timestamp: at(), Notification: n.class, Channel: n.channel, Notifiable: n.notifiable, Status: "sent"}
			if rng.Float64() < failRate(n.class, n.failRate) {
				it.Status, it.Error = "failed", &ingest.ItemError{Type: n.errType, Message: n.errMsg}
			}
			b.Notifications = append(b.Notifications, it)
		}
	}
	for _, m := range s.mails {
		for range poisson(rng, m.perHour*load) {
			it := ingest.MailItem{ID: newUUID(), Timestamp: at(), Mailable: m.mailable, Mailer: m.mailer, Recipients: m.recipients, Status: "sent"}
			if rng.Float64() < failRate(m.mailable, m.failRate) {
				it.Status, it.Error = "failed", &ingest.ItemError{Type: m.errType, Message: m.errMsg}
			}
			b.Mails = append(b.Mails, it)
		}
	}
	for _, c := range s.commands {
		runs := 0
		switch {
		case c.everyHours > 0 && hour.Hour()%c.everyHours == 0:
			runs = 1
		case c.everyHours == 0 && rng.Float64() < 0.02*scale: // by hand, about every two days
			runs = 1
		}
		for range runs {
			exit := 0
			it := ingest.CommandItem{ID: newUUID(), Timestamp: hour.Add(time.Duration(rng.IntN(3)) * time.Second).Format(time.RFC3339Nano), Command: c.command}
			if hour.After(now) {
				it.Timestamp = now.Format(time.RFC3339Nano)
			}
			it.DurationMs = int64(c.durationMs * math.Exp(rng.NormFloat64()*0.35))
			if rng.Float64() < failRate(c.command, c.failRate) {
				exit = 1
				if c.exitCode != 0 {
					exit = c.exitCode
				}
				if c.errType != "" {
					it.Error = &ingest.ItemError{Type: c.errType, Message: c.errMsg}
				}
				it.DurationMs /= 3 // failures usually stop early
			}
			it.ExitCode = &exit
			b.Commands = append(b.Commands, it)
		}
	}
	for _, c := range s.cache {
		reads := int64(math.Round(c.readsPerHour * load * (0.8 + 0.4*rng.Float64())))
		if reads == 0 {
			continue
		}
		hitRate := c.hitRate
		if inIncident && s.hit[c.key] {
			hitRate *= 0.5 // writes fail, so reads miss
		}
		hits := int64(math.Round(float64(reads) * math.Min(1, hitRate+rng.NormFloat64()*0.01)))
		hits = max(0, min(reads, hits))
		misses := reads - hits
		writes := misses // remember(): every miss writes
		failures := int64(math.Round(float64(writes) * failRate(c.key, c.failRate)))
		writes -= failures
		b.Cache = append(b.Cache, ingest.CacheItem{
			Timestamp: hour.Add(59 * time.Minute).Format(time.RFC3339Nano), Store: c.store, Key: c.key,
			Hits: hits, Misses: misses, Writes: writes, Deletes: int64(math.Round(float64(reads) * c.deletesShare)), Failures: failures,
		})
		if hour.Add(59 * time.Minute).After(now) {
			b.Cache[len(b.Cache)-1].Timestamp = now.Format(time.RFC3339Nano)
		}
	}
	return b
}

// diurnal is the share of the daily peak an hour gets: quiet at night (UTC),
// busiest in the afternoon, a little quieter at weekends.
func diurnal(t time.Time) float64 {
	f := 0.35 + 0.65*math.Pow(math.Sin(math.Pi*float64((t.Hour()+18)%24)/24), 2)
	if wd := t.Weekday(); wd == time.Saturday || wd == time.Sunday {
		f *= 0.7
	}
	return f
}

// poisson draws how many times something with the given rate happens.
func poisson(rng *rand.Rand, mean float64) int {
	if mean <= 0 {
		return 0
	}
	if mean > 30 {
		return max(0, int(math.Round(mean+math.Sqrt(mean)*rng.NormFloat64())))
	}
	l, k, p := math.Exp(-mean), 0, 1.0
	for {
		p *= rng.Float64()
		if p <= l {
			return k
		}
		k++
	}
}
