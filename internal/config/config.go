// Package config loads Faultbox settings from FAULTBOX_* environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MinSecretLength is the minimum length of FAULTBOX_SECRET in bytes (D17).
const MinSecretLength = 32

// MinPasswordLength applies to dashboard user passwords.
const MinPasswordLength = 12

// Config is the fully validated runtime configuration.
type Config struct {
	Port         string
	DatabasePath string
	Secret       []byte
	LogLevel     slog.Level

	// TrustProxy makes the server honour X-Forwarded-For and
	// X-Forwarded-Proto. Enable it only behind a reverse proxy you control.
	TrustProxy bool

	RetentionDays int
	SessionTTL    time.Duration

	// AdminEmail and AdminPassword create the first dashboard user on
	// startup when no users exist yet (D9).
	AdminEmail    string
	AdminPassword string

	// PublicURL is the external base URL (e.g. https://errors.example.com),
	// shown to users as the DSN for their Laravel apps. Optional.
	PublicURL string

	// BcryptCost is not read from the environment. Tests lower it.
	BcryptCost int

	ShutdownTimeout time.Duration
}

// Load reads configuration using getenv (normally os.Getenv).
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		Port:            or(getenv("FAULTBOX_PORT"), "8080"),
		DatabasePath:    or(getenv("FAULTBOX_DATABASE_PATH"), "faultbox.db"),
		Secret:          []byte(getenv("FAULTBOX_SECRET")),
		AdminEmail:      strings.TrimSpace(getenv("FAULTBOX_ADMIN_EMAIL")),
		AdminPassword:   getenv("FAULTBOX_ADMIN_PASSWORD"),
		PublicURL:       strings.TrimSuffix(strings.TrimSpace(getenv("FAULTBOX_PUBLIC_URL")), "/"),
		RetentionDays:   30,
		SessionTTL:      7 * 24 * time.Hour,
		BcryptCost:      12,
		ShutdownTimeout: 10 * time.Second,
	}

	var errs []error

	if v := getenv("FAULTBOX_LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("FAULTBOX_LOG_LEVEL: %w", err))
		}
	}
	if v := getenv("FAULTBOX_TRUST_PROXY"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("FAULTBOX_TRUST_PROXY: %q is not a boolean", v))
		}
		cfg.TrustProxy = b
	}
	if v := getenv("FAULTBOX_RETENTION_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			errs = append(errs, fmt.Errorf("FAULTBOX_RETENTION_DAYS: %q must be a whole number of days, at least 1", v))
		}
		cfg.RetentionDays = n
	}
	if v := getenv("FAULTBOX_SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("FAULTBOX_SHUTDOWN_TIMEOUT: %q must be a positive duration such as 10s", v))
		}
		cfg.ShutdownTimeout = d
	}

	if err := cfg.Validate(); err != nil {
		errs = append(errs, err)
	}
	return cfg, errors.Join(errs...)
}

// Validate checks invariants that hold however the Config was built.
func (c Config) Validate() error {
	var errs []error
	if len(c.Secret) < MinSecretLength {
		errs = append(errs, fmt.Errorf("FAULTBOX_SECRET must be set and at least %d bytes (generate one with: openssl rand -base64 48)", MinSecretLength))
	}
	if p, err := strconv.Atoi(c.Port); err != nil || p < 1 || p > 65535 {
		errs = append(errs, fmt.Errorf("FAULTBOX_PORT: %q is not a valid port", c.Port))
	}
	if c.DatabasePath == "" {
		errs = append(errs, errors.New("FAULTBOX_DATABASE_PATH must not be empty"))
	}
	if c.PublicURL != "" {
		if u, err := url.Parse(c.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("FAULTBOX_PUBLIC_URL: %q must be an absolute http(s) URL", c.PublicURL))
		}
	}
	if (c.AdminEmail == "") != (c.AdminPassword == "") {
		errs = append(errs, errors.New("set both FAULTBOX_ADMIN_EMAIL and FAULTBOX_ADMIN_PASSWORD, or neither"))
	}
	if c.AdminEmail != "" {
		if _, err := mail.ParseAddress(c.AdminEmail); err != nil {
			errs = append(errs, fmt.Errorf("FAULTBOX_ADMIN_EMAIL: %q is not a valid email address", c.AdminEmail))
		}
		if len(c.AdminPassword) < MinPasswordLength {
			errs = append(errs, fmt.Errorf("FAULTBOX_ADMIN_PASSWORD must be at least %d characters", MinPasswordLength))
		}
	}
	return errors.Join(errs...)
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
