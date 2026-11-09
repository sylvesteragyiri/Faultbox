package unit

import (
	"strings"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/config"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestConfigDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(map[string]string{"FAULTBOX_SECRET": testSecret}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "8080" || cfg.DatabasePath != "faultbox.db" {
		t.Fatalf("unexpected defaults: port %q, db %q", cfg.Port, cfg.DatabasePath)
	}
	if cfg.RetentionDays != 30 || cfg.SessionTTL != 7*24*time.Hour || cfg.BcryptCost != 12 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.TrustProxy {
		t.Fatal("TrustProxy must default to false")
	}
}

func TestConfigReadsEnvironment(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(map[string]string{
		"FAULTBOX_SECRET":           testSecret,
		"FAULTBOX_PORT":             "9000",
		"FAULTBOX_DATABASE_PATH":    "/data/x.db",
		"FAULTBOX_TRUST_PROXY":      "true",
		"FAULTBOX_RETENTION_DAYS":   "14",
		"FAULTBOX_SHUTDOWN_TIMEOUT": "3s",
		"FAULTBOX_LOG_LEVEL":        "debug",
		"FAULTBOX_PUBLIC_URL":       "https://errors.example.com/",
		"FAULTBOX_ADMIN_EMAIL":      "me@example.com",
		"FAULTBOX_ADMIN_PASSWORD":   "a-long-enough-password",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "9000" || cfg.DatabasePath != "/data/x.db" || !cfg.TrustProxy ||
		cfg.RetentionDays != 14 || cfg.ShutdownTimeout != 3*time.Second ||
		cfg.PublicURL != "https://errors.example.com" || cfg.AdminEmail != "me@example.com" {
		t.Fatalf("environment not applied: %+v", cfg)
	}
}

func TestConfigRejectsBadValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		vars    map[string]string
		wantErr string
	}{
		{"missing secret", map[string]string{}, "FAULTBOX_SECRET"},
		{"short secret", map[string]string{"FAULTBOX_SECRET": "short"}, "FAULTBOX_SECRET"},
		{"bad port", map[string]string{"FAULTBOX_SECRET": testSecret, "FAULTBOX_PORT": "http"}, "FAULTBOX_PORT"},
		{"bad bool", map[string]string{"FAULTBOX_SECRET": testSecret, "FAULTBOX_TRUST_PROXY": "maybe"}, "FAULTBOX_TRUST_PROXY"},
		{"zero retention", map[string]string{"FAULTBOX_SECRET": testSecret, "FAULTBOX_RETENTION_DAYS": "0"}, "FAULTBOX_RETENTION_DAYS"},
		{"relative public url", map[string]string{"FAULTBOX_SECRET": testSecret, "FAULTBOX_PUBLIC_URL": "errors.example.com"}, "FAULTBOX_PUBLIC_URL"},
		{"email without password", map[string]string{"FAULTBOX_SECRET": testSecret, "FAULTBOX_ADMIN_EMAIL": "me@example.com"}, "or neither"},
		{"short admin password", map[string]string{"FAULTBOX_SECRET": testSecret, "FAULTBOX_ADMIN_EMAIL": "me@example.com", "FAULTBOX_ADMIN_PASSWORD": "short"}, "FAULTBOX_ADMIN_PASSWORD"},
		{"bad admin email", map[string]string{"FAULTBOX_SECRET": testSecret, "FAULTBOX_ADMIN_EMAIL": "nope", "FAULTBOX_ADMIN_PASSWORD": "a-long-enough-password"}, "FAULTBOX_ADMIN_EMAIL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load(env(tt.vars))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want mention of %q", err, tt.wantErr)
			}
		})
	}
}
