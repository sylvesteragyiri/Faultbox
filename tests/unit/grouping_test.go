package unit

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sylvesteragyiri/faultbox/internal/grouping"
	"github.com/sylvesteragyiri/faultbox/internal/ingest"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

func TestNormalizeMessage(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"No query results for model [App\\Models\\Order] 48213", "No query results for model [App\\Models\\Order] <n>"},
		{"User 9f8c2a1e-5b7d-4c3a-9e21-7a6b5c4d3e2f not found", "User <uuid> not found"},
		{"Mail to jane.doe@example.com bounced", "Mail to <email> bounced"},
		{"Token expired at 2026-11-09T10:14:03Z", "Token expired at <date>"},
		{"Lock wait 2026-11-09 10:14:03 exceeded", "Lock wait <date> exceeded"},
		{"Connection refused from 10.0.12.7", "Connection refused from <ip>"},
		{"Cache key 5f4dcc3b5aa765d61d8327deb882cf99 missing", "Cache key <hex> missing"},
		{"Pointer 0x7ffd5c2e0a10 invalid", "Pointer <hex> invalid"},
		{"Duplicate entry 'jane@x.io' for key", "Duplicate entry '<s>' for key"},
		{"Undefined array key \"shipping_address\"", "Undefined array key \"shipping_address\""},
		{"Call to a member function bad() on null", "Call to a member function bad() on null"},
		{"  lots   of\n\twhitespace  ", "lots of whitespace"},
		{
			"SQLSTATE[23000]: Integrity constraint violation (Connection: mysql, SQL: insert into `users` (`email`) values (a@b.co))",
			"SQLSTATE[<n>]: Integrity constraint violation (SQL: insert into `users` (`email`) …)",
		},
		{
			"SQLSTATE[HY000]: General error (Connection: pgsql, SQL: update \"orders\" set \"status\" = paid where \"id\" = 7)",
			"SQLSTATE[HY000]: General error (SQL: update \"orders\" …)",
		},
	}
	for _, tt := range tests {
		if got := grouping.NormalizeMessage(tt.in); got != tt.want {
			t.Errorf("NormalizeMessage(%q)\n got  %q\n want %q", tt.in, got, tt.want)
		}
	}
}

func TestNormalizePathAndInApp(t *testing.T) {
	t.Parallel()

	paths := map[string]string{
		"/var/www/releases/20261109101200/app/Models/User.php": "app/Models/User.php",
		"/var/www/html/vendor/laravel/framework/src/X.php":     "vendor/laravel/framework/src/X.php",
		`C:\inetpub\shop\app\Http\Kernel.php`:                  "app/Http/Kernel.php",
		"/srv/artisan":                                         "artisan",
	}
	for in, want := range paths {
		if got := grouping.NormalizePath(in); got != want {
			t.Errorf("NormalizePath(%q) = %q, want %q", in, got, want)
		}
	}

	yes, no := true, false
	inApp := []struct {
		f    grouping.Frame
		want bool
	}{
		{grouping.Frame{File: "/var/www/app/Jobs/Sync.php"}, true},
		{grouping.Frame{File: "/var/www/vendor/laravel/framework/src/Queue/Worker.php"}, false},
		{grouping.Frame{File: `C:\shop\vendor\guzzle\Client.php`}, false},
		{grouping.Frame{File: "/var/www/vendor/acme/pkg/A.php", InApp: &yes}, true},
		{grouping.Frame{File: "/var/www/app/A.php", InApp: &no}, false},
		{grouping.Frame{Function: "{closure}"}, false},
	}
	for _, tt := range inApp {
		if got := grouping.IsInApp(tt.f); got != tt.want {
			t.Errorf("IsInApp(%+v) = %v, want %v", tt.f, got, tt.want)
		}
	}
}

// fixtureException decodes a fixture through the real payload decoder.
func fixtureException(t *testing.T, p map[string]any) (string, string, []grouping.Frame) {
	t.Helper()
	payload, err := ingest.Decode(bytes.NewReader(testutil.MustJSON(t, p)))
	if err != nil {
		t.Fatal(err)
	}
	return payload.Exception.Type, payload.Exception.Message, payload.Exception.Frames
}

func fingerprintOf(t *testing.T, p map[string]any) string {
	t.Helper()
	typ, msg, frames := fixtureException(t, p)
	return grouping.Fingerprint(typ, msg, frames)
}

func TestFixturesHaveDistinctFingerprints(t *testing.T) {
	t.Parallel()

	seen := map[string]string{}
	for _, name := range testutil.FixtureNames {
		fp := fingerprintOf(t, testutil.Fixture(t, name))
		if len(fp) != 64 {
			t.Fatalf("%s: fingerprint %q is not a SHA-256 hex digest", name, fp)
		}
		if other, dup := seen[fp]; dup {
			t.Fatalf("%s and %s share a fingerprint", name, other)
		}
		seen[fp] = name
	}
}

// Variants of the same error that must stay in one issue.
func TestFingerprintIgnoresDynamicValues(t *testing.T) {
	t.Parallel()

	replaceMsg := func(from, to string) func(map[string]any) {
		return func(p map[string]any) {
			ex := testutil.Exception(p)
			ex["message"] = strings.ReplaceAll(ex["message"].(string), from, to)
		}
	}
	shiftLines := func(p map[string]any) {
		for _, f := range testutil.Frames(p) {
			f.(map[string]any)["line"] = f.(map[string]any)["line"].(float64) + 17
		}
	}
	newRelease := func(from, to string) func(map[string]any) {
		return func(p map[string]any) {
			for _, f := range testutil.Frames(p) {
				fm := f.(map[string]any)
				fm["file"] = strings.ReplaceAll(fm["file"].(string), from, to)
			}
			replaceMsg(from, to)(p)
		}
	}

	tests := []struct {
		fixture string
		name    string
		mutate  func(map[string]any)
	}{
		{"unique_violation", "different email and name", func(p map[string]any) {
			replaceMsg("jane.doe@example.com", "sam.lee@example.org")(p)
			replaceMsg("Jane Doe", "Sam Lee")(p)
		}},
		{"unique_violation", "different timestamps and hash", func(p map[string]any) {
			replaceMsg("2026-11-09 10:14:03", "2027-01-02 23:59:59")(p)
			replaceMsg("Qm9vZ3VzSGFzaFZhbHVlLi4u", "T3RoZXJIYXNoVmFsdWVzLi4u")(p)
		}},
		{"unique_violation", "new deploy directory", newRelease("20261109101200", "20261201080000")},
		{"model_not_found", "different id", replaceMsg("48213", "7")},
		{"model_not_found", "code moved down 17 lines", shiftLines},
		{"type_error", "different caller line and release dir", func(p map[string]any) {
			replaceMsg("on line 42", "on line 57")(p)
			newRelease("20261109093000", "20261110120000")(p)
		}},
		{"job_timeout", "different invoice and timing", func(p map[string]any) {
			replaceMsg("INV-0042", "INV-9917")(p)
			replaceMsg("30001", "30004")(p)
		}},
		{"max_attempts", "vendor line numbers change on upgrade", shiftLines},
	}
	for _, tt := range tests {
		t.Run(tt.fixture+"/"+tt.name, func(t *testing.T) {
			t.Parallel()
			base := fingerprintOf(t, testutil.Fixture(t, tt.fixture))
			p := testutil.Fixture(t, tt.fixture)
			tt.mutate(p)
			if got := fingerprintOf(t, p); got != base {
				t.Fatal("fingerprint changed; these events would split into two issues")
			}
		})
	}
}

// Changes that must produce a separate issue.
func TestFingerprintSeparatesDifferentOrigins(t *testing.T) {
	t.Parallel()

	setFrame := func(i int, key, value string) func(map[string]any) {
		return func(p map[string]any) { testutil.Frames(p)[i].(map[string]any)[key] = value }
	}
	tests := []struct {
		fixture string
		name    string
		mutate  func(map[string]any)
	}{
		{"model_not_found", "thrown from another controller action", setFrame(2, "function", "edit")},
		{"model_not_found", "thrown from another controller", setFrame(2, "class", "App\\Http\\Controllers\\InvoiceController")},
		{"model_not_found", "different model", func(p map[string]any) {
			testutil.Exception(p)["message"] = "No query results for model [App\\Models\\Invoice] 48213"
		}},
		{"undefined_array_key", "different missing key", func(p map[string]any) {
			testutil.Exception(p)["message"] = "Undefined array key \"billing_address\""
		}},
		{"undefined_array_key", "different exception class", func(p map[string]any) {
			testutil.Exception(p)["type"] = "App\\Exceptions\\CheckoutException"
		}},
		{"unique_violation", "different table", func(p map[string]any) {
			ex := testutil.Exception(p)
			ex["message"] = strings.Replace(ex["message"].(string), "insert into `users`", "insert into `teams`", 1)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture+"/"+tt.name, func(t *testing.T) {
			t.Parallel()
			base := fingerprintOf(t, testutil.Fixture(t, tt.fixture))
			p := testutil.Fixture(t, tt.fixture)
			tt.mutate(p)
			if got := fingerprintOf(t, p); got == base {
				t.Fatal("fingerprint unchanged; distinct errors would be merged into one issue")
			}
		})
	}
}

func TestQueueFingerprintIncludesJob(t *testing.T) {
	t.Parallel()

	ex := fingerprintOf(t, testutil.Fixture(t, "job_timeout"))
	a := grouping.QueueFingerprint("App\\Jobs\\SyncInvoiceToXero", ex)
	b := grouping.QueueFingerprint("App\\Jobs\\SyncContactToXero", ex)
	if a == b || a == ex {
		t.Fatal("queue fingerprint must differ per job and from the plain exception fingerprint")
	}
	if a != grouping.QueueFingerprint("App\\Jobs\\SyncInvoiceToXero", ex) {
		t.Fatal("queue fingerprint is not deterministic")
	}
}

func TestCulprit(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"unique_violation":    "App\\Actions\\Fortify\\CreateNewUser::create",
		"model_not_found":     "App\\Http\\Controllers\\OrderController::show",
		"undefined_array_key": "App\\Services\\CheckoutService::buildShipment",
		"job_timeout":         "App\\Services\\Xero\\InvoiceClient::push",
		"max_attempts":        "Illuminate\\Queue\\Worker::maxAttemptsExceededException",
	}
	for name, want := range tests {
		_, _, frames := fixtureException(t, testutil.Fixture(t, name))
		if got := grouping.Culprit(frames); got != want {
			t.Errorf("%s: culprit %q, want %q", name, got, want)
		}
	}
	file := []grouping.Frame{{File: "/var/www/html/app/helpers.php", Line: 12}}
	if got := grouping.Culprit(file); got != "app/helpers.php:12" {
		t.Errorf("file-only culprit = %q", got)
	}
}

// Fixtures must stay valid JSON in the documented wire format.
func TestFixturesDecode(t *testing.T) {
	t.Parallel()
	for _, name := range testutil.FixtureNames {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(testutil.FixtureBytes(t, "laravel/"+name+".json"), &raw); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, ok := raw["exception"]; !ok {
			t.Fatalf("%s: no exception", name)
		}
	}
}
