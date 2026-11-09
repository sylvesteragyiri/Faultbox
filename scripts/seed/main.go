// Command seed fills a local Faultbox database with realistic demo data for
// working on the dashboard. It runs every event through the real ingest
// pipeline (Decode, Validate, Queue, Processor), so grouping, culprits,
// scrubbing and queue failures behave exactly as live traffic would. The only
// difference is that each event is validated against its own backdated
// clock, which lets the data span weeks instead of the API's 24 hours.
//
//	go run ./scripts/seed                    # data/faultbox.db, ~1,500 events over 28 days
//	go run ./scripts/seed -events 5000 -days 14
//	go run ./scripts/seed -db path/to/other.db
//
// It is safe to run while the server is up (SQLite WAL). Running it again
// reuses the demo projects and adds more events.
package main

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/internal/ingest"
	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// project is a demo project and the environments it accepts.
type project struct {
	name string
	envs []string
	id   int64
}

// shape is how an issue's events spread over the seeded period.
type shape int

const (
	steady   shape = iota // roughly even, every day
	growing               // more events toward today
	spike                 // a burst a few days ago, quiet since
	newToday              // first seen in the last day
	stopped               // happened in the first half, then stopped
)

// issue is one demo issue: a payload template, its project and traffic.
type issue struct {
	fixture string          // base fixture in tests/fixtures/laravel
	project int             // index into projects
	weight  float64         // share of all events
	shape   shape           //
	status  string          // final status to set ("" keeps unresolved)
	edit    func(m payload) // changes that make it a distinct issue
	kind    string          // ingest.KindException or ingest.KindQueueFailure
	tmpl    payload         // loaded template
	id      int64           // issue ID once stored
}

type payload = map[string]any

var projects = []*project{
	{name: "Shop API", envs: []string{"production", "staging"}},
	{name: "Billing Worker", envs: []string{"production"}},
	{name: "Marketing Site"}, // accepts any environment
}

// releases from oldest to newest; events get the release live at their time.
var releases = []string{"2026.10.3", "2026.10.4", "2026.11.0", "2026.11.1"}

func issues() []*issue {
	return []*issue{
		// Shop API
		{fixture: "type_error", project: 0, weight: 9, shape: growing},
		{fixture: "undefined_array_key", project: 0, weight: 6, shape: steady},
		{fixture: "model_not_found", project: 0, weight: 12, shape: steady},
		{fixture: "unique_violation", project: 0, weight: 4, shape: spike, status: store.IssueResolved},
		{fixture: "type_error", project: 0, weight: 2, shape: newToday, edit: func(m payload) {
			setException(m, "Illuminate\\Validation\\ValidationException", "The coupon code field must be a string.",
				"App\\Http\\Controllers\\CouponController", "apply", "app/Http/Controllers/CouponController.php", 31)
			setRequest(m, "POST", "https://shop.example.com/cart/coupon", "cart.coupon", 422)
		}},
		{fixture: "model_not_found", project: 0, weight: 3, shape: stopped, status: store.IssueIgnored, edit: func(m payload) {
			setException(m, "Symfony\\Component\\HttpKernel\\Exception\\NotFoundHttpException", "The route wp-login.php could not be found.",
				"Illuminate\\Routing\\RouteCollection", "match", "vendor/laravel/framework/src/Illuminate/Routing/RouteCollection.php", 45)
			setRequest(m, "GET", "https://shop.example.com/wp-login.php", "", 404)
		}},
		{fixture: "type_error", project: 0, weight: 5, shape: growing, edit: func(m payload) {
			setException(m, "Illuminate\\Database\\QueryException",
				"SQLSTATE[HY000] [2002] Connection refused (Connection: mysql, SQL: select * from `products` where `slug` = summer-sale limit 1)",
				"App\\Repositories\\ProductRepository", "findBySlug", "app/Repositories/ProductRepository.php", 58)
			setRequest(m, "GET", "https://shop.example.com/products/summer-sale", "products.show", 500)
		}},

		// Billing Worker (queue jobs)
		{fixture: "job_timeout", project: 1, weight: 8, shape: spike, kind: ingest.KindQueueFailure},
		{fixture: "max_attempts", project: 1, weight: 5, shape: steady, kind: ingest.KindQueueFailure},
		{fixture: "job_timeout", project: 1, weight: 4, shape: growing, kind: ingest.KindQueueFailure, edit: func(m payload) {
			setException(m, "Stripe\\Exception\\CardException", "Your card was declined.",
				"App\\Services\\Billing\\ChargeCustomer", "charge", "app/Services/Billing/ChargeCustomer.php", 64)
			setJob(m, "App\\Jobs\\ChargeSubscription", "billing")
		}},
		{fixture: "job_timeout", project: 1, weight: 2, shape: newToday, kind: ingest.KindQueueFailure, edit: func(m payload) {
			setException(m, "Illuminate\\Mail\\Transport\\TransportException", "Expected response code 250 but got code \"421\", with message \"421 Too many connections\"",
				"App\\Mail\\InvoicePaid", "send", "app/Mail/InvoicePaid.php", 27)
			setJob(m, "App\\Jobs\\SendInvoiceEmail", "mail")
		}},
		{fixture: "undefined_array_key", project: 1, weight: 3, shape: stopped, status: store.IssueResolved, edit: func(m payload) {
			setException(m, "DivisionByZeroError", "Division by zero",
				"App\\Services\\Billing\\ProrationCalculator", "dailyRate", "app/Services/Billing/ProrationCalculator.php", 22)
			delete(m, "request")
		}},

		// Marketing Site
		{fixture: "undefined_array_key", project: 2, weight: 3, shape: steady, edit: func(m payload) {
			setException(m, "ErrorException", "Attempt to read property \"title\" on null",
				"App\\View\\Components\\BlogCard", "render", "app/View/Components/BlogCard.php", 19)
			setRequest(m, "GET", "https://example.com/blog/launch-week", "blog.show", 500)
		}},
		{fixture: "model_not_found", project: 2, weight: 2, shape: spike, edit: func(m payload) {
			setException(m, "Illuminate\\Http\\Client\\ConnectionException", "cURL error 6: Could not resolve host: api.newsletter.example",
				"App\\Actions\\SubscribeToNewsletter", "handle", "app/Actions/SubscribeToNewsletter.php", 40)
			setRequest(m, "POST", "https://example.com/newsletter", "newsletter.subscribe", 500)
		}},
	}
}

func main() {
	defaultDB := os.Getenv("FAULTBOX_DATABASE_PATH")
	if defaultDB == "" {
		defaultDB = "data/faultbox.db"
	}
	dbPath := flag.String("db", defaultDB, "database file")
	total := flag.Int("events", 1500, "number of events to add")
	days := flag.Int("days", 28, "spread events over this many days (keep under the retention period)")
	fixtures := flag.String("fixtures", "tests/fixtures/laravel", "directory of Laravel event fixtures")
	flag.Parse()

	if err := run(*dbPath, *fixtures, *total, *days); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func run(dbPath, fixtureDir string, total, days int) error {
	ctx := context.Background()
	if days < 1 || total < 1 {
		return fmt.Errorf("-events and -days must be positive")
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	keys, err := ensureProjects(ctx, st)
	if err != nil {
		return err
	}

	list := issues()
	var weights float64
	for _, is := range list {
		b, err := os.ReadFile(filepath.Join(fixtureDir, is.fixture+".json")) //nolint:gosec // G304: dev tool reading the fixture directory it was given
		if err != nil {
			return fmt.Errorf("read fixture (run from the repo root or pass -fixtures): %w", err)
		}
		if err := json.Unmarshal(b, &is.tmpl); err != nil {
			return fmt.Errorf("parse fixture %s: %w", is.fixture, err)
		}
		if is.edit != nil {
			is.edit(is.tmpl)
		}
		if is.kind == "" {
			is.kind = ingest.KindException
		}
		weights += is.weight
	}

	// Demo-data shape only; nothing here needs to be unpredictable.
	rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 7)) //nolint:gosec // G404: see above

	// Each issue's first event carries a known marker ID, so the result hook
	// can learn the issue's ID. The map is complete before the processor
	// starts and only read afterwards, so the collector goroutine can use it
	// without locking.
	byUUID := make(map[string]*issue, len(list))
	markers := make([]string, len(list))
	for i, is := range list {
		markers[i] = newUUID()
		byUUID[markers[i]] = is
	}

	// The real pipeline. A bounded queue keeps memory flat however many
	// events are seeded; when it fills up we wait for the processor to drain it.
	results := make(chan []store.IngestResult, 64)
	q := ingest.NewQueue(5000, 256<<20)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := &ingest.Metrics{}
	proc := ingest.NewProcessor(q, st, metrics, logger, ingest.ProcessorOptions{
		BatchSize: 500,
		OnResults: func(r []store.IngestResult) { results <- r },
	})
	proc.Start()
	collected := make(chan struct{})
	go func() {
		for batch := range results {
			for _, r := range batch {
				if is := byUUID[r.UUID]; is != nil && r.IssueID != 0 {
					is.id = r.IssueID
				}
			}
		}
		close(collected)
	}()

	now := time.Now().UTC()
	period := time.Duration(days) * 24 * time.Hour
	rejected := 0
	started := time.Now()
	lastReport := started
	for i, is := range list {
		n := max(1, int(math.Round(float64(total)*is.weight/weights)))
		p := projects[is.project]
		for j := range n {
			at := now.Add(-pickAge(rng, is.shape, period))
			m := clone(is.tmpl)
			id := newUUID()
			if j == 0 {
				id = markers[i]
			}
			m["event_id"] = id
			m["timestamp"] = at.Format(time.RFC3339Nano)
			m["release"] = releaseAt(at, now, period)
			if len(p.envs) > 0 {
				m["environment"] = p.envs[0]
				if len(p.envs) > 1 && rng.IntN(5) == 0 {
					m["environment"] = p.envs[1]
				}
			} else if rng.IntN(6) == 0 {
				m["environment"] = "staging"
			} else {
				m["environment"] = "production"
			}
			if job, ok := m["job"].(payload); ok {
				job["id"] = newUUID()
				// Some runs are retries that failed and will run again.
				if rng.IntN(5) < 2 {
					tries := 3
					if mt, ok := job["max_tries"].(float64); ok && mt > 1 {
						tries = int(mt)
					}
					job["kind"] = "attempt"
					job["attempts"] = 1 + rng.IntN(tries-1)
				}
			}

			body, _ := json.Marshal(m)
			pl, err := ingest.Decode(bytes.NewReader(body))
			if err == nil {
				var ev *ingest.Event
				// Validate against a clock just after the event, so old timestamps pass.
				ev, err = ingest.Validate(pl, is.kind, p.id, p.envs, at.Add(time.Minute), len(body))
				if err == nil {
					err = enqueue(ctx, q, proc, ev)
				}
			}
			if err == nil {
				if time.Since(lastReport) > 5*time.Second {
					lastReport = time.Now()
					rate := float64(metrics.Processed.Load()) / time.Since(started).Seconds()
					fmt.Fprintf(os.Stderr, "  %d / %d events stored (%.0f/s)\n", metrics.Processed.Load(), total, rate)
				}
			} else {
				rejected++
				if rejected <= 3 {
					fmt.Fprintf(os.Stderr, "skipped an event of %s: %v\n", is.fixture, err)
				}
			}
		}
	}

	if err := proc.Flush(ctx); err != nil {
		return err
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := proc.Shutdown(shutdownCtx); err != nil {
		return err
	}
	close(results)
	<-collected

	for _, is := range list {
		if is.status != "" && is.id != 0 {
			if err := st.SetIssueStatus(ctx, is.id, is.status); err != nil {
				return err
			}
		}
	}

	fmt.Printf("Seeded %d events into %d issues across %d projects (%d days) in %s\n",
		metrics.Processed.Load(), len(list), len(projects), days, dbPath)
	if f := metrics.Failed.Load(); f > 0 || rejected > 0 {
		fmt.Printf("  %d failed to store, %d rejected by validation\n", f, rejected)
	}
	for _, p := range projects {
		if k := keys[p.name]; k != "" {
			fmt.Printf("  New project %-15s API key %s\n", p.name, k)
		}
	}
	return nil
}

// enqueue adds an event, waiting for the processor to drain the queue
// whenever it is full.
func enqueue(ctx context.Context, q *ingest.Queue, proc *ingest.Processor, ev *ingest.Event) error {
	for {
		err := q.Enqueue(ev)
		if !errors.Is(err, ingest.ErrQueueFull) {
			return err
		}
		if err := proc.Flush(ctx); err != nil {
			return err
		}
	}
}

// ensureProjects finds or creates the demo projects. It returns the API key
// of each project it created; existing projects keep their keys.
func ensureProjects(ctx context.Context, st *store.Store) (map[string]string, error) {
	existing, err := st.Projects(ctx)
	if err != nil {
		return nil, err
	}
	keys := map[string]string{}
	for _, p := range projects {
		for _, e := range existing {
			if strings.EqualFold(e.Name, p.name) {
				p.id = e.ID
				p.envs = e.Environments
			}
		}
		if p.id != 0 {
			continue
		}
		key, hash, hint, err := auth.NewAPIKey()
		if err != nil {
			return nil, err
		}
		created, err := st.CreateProject(ctx, p.name, p.envs, hash, hint)
		if err != nil {
			return nil, fmt.Errorf("create project %s: %w", p.name, err)
		}
		p.id = created.ID
		keys[p.name] = key
	}
	return keys, nil
}

// pickAge returns how long ago an event happened, following the issue's shape.
func pickAge(rng *rand.Rand, s shape, period time.Duration) time.Duration {
	f := rng.Float64()
	switch s {
	case growing:
		f = 1 - math.Sqrt(rng.Float64()) // denser near now
	case spike:
		center := 0.15 + 0.1*rng.NormFloat64()*0.3
		f = math.Min(math.Max(center+rng.NormFloat64()*0.03, 0.08), 0.35)
	case newToday:
		return time.Duration(rng.Float64() * float64(20*time.Hour))
	case stopped:
		f = 0.5 + rng.Float64()*0.5
	}
	return time.Duration(f * float64(period))
}

// releaseAt picks the release that was live at the event's time.
func releaseAt(at, now time.Time, period time.Duration) string {
	age := float64(now.Sub(at)) / float64(period) // 0 = now, 1 = start
	i := len(releases) - 1 - int(age*float64(len(releases)))
	return releases[max(0, min(len(releases)-1, i))]
}

func setException(m payload, typ, msg, class, fn, file string, line int) {
	ex := m["exception"].(payload)
	ex["type"] = typ
	ex["message"] = msg
	frames, _ := ex["frames"].([]any)
	top := payload{"file": "/var/www/html/" + file, "line": line, "function": fn, "class": class}
	// Keep the framework frames below so traces look real.
	var rest []any
	for _, f := range frames {
		if fm, ok := f.(payload); ok && strings.Contains(fmt.Sprint(fm["file"]), "/vendor/") {
			rest = append(rest, f)
		}
	}
	ex["frames"] = append([]any{top}, rest...)
}

func setRequest(m payload, method, url, route string, status int) {
	req, ok := m["request"].(payload)
	if !ok {
		req = payload{}
		m["request"] = req
	}
	req["method"] = method
	req["url"] = url
	req["status_code"] = status
	if route != "" {
		req["route"] = route
	} else {
		delete(req, "route")
	}
}

func setJob(m payload, name, queue string) {
	if job, ok := m["job"].(payload); ok {
		job["name"] = name
		job["queue"] = queue
	}
}

func clone(m payload) payload {
	b, _ := json.Marshal(m)
	var out payload
	_ = json.Unmarshal(b, &out)
	return out
}

func newUUID() string {
	var b [16]byte
	_, _ = crand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
