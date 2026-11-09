// Package app wires configuration, storage and HTTP handlers into a running
// Faultbox instance. Both the binary and the feature tests start it here.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"

	"github.com/sylvesteragyiri/faultbox/internal/api"
	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/internal/config"
	"github.com/sylvesteragyiri/faultbox/internal/dashboard"
	"github.com/sylvesteragyiri/faultbox/internal/server"
	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// App is a configured Faultbox instance.
type App struct {
	cfg     config.Config
	logger  *slog.Logger
	store   *store.Store
	handler http.Handler
}

// New opens the database, applies migrations, creates the bootstrap admin
// if configured, and builds the HTTP handler.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger, version string) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	keys, err := auth.DeriveKeys(cfg.Secret)
	if err != nil {
		return nil, fmt.Errorf("derive keys: %w", err)
	}
	st, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return nil, err
	}

	a := &App{cfg: cfg, logger: logger, store: st}
	if err := a.bootstrapAdmin(ctx); err != nil {
		_ = st.Close()
		return nil, err
	}

	dash, err := dashboard.New(dashboard.Options{
		Store:         st,
		Logger:        logger,
		CSRFKey:       keys.CSRF,
		SessionTTL:    cfg.SessionTTL,
		TrustProxy:    cfg.TrustProxy,
		Version:       version,
		RetentionDays: cfg.RetentionDays,
		PublicURL:     cfg.PublicURL,
	})
	if err != nil {
		_ = st.Close()
		return nil, err
	}

	a.handler = server.New(server.Options{
		Logger:    logger,
		API:       api.New(st, logger),
		Dashboard: dash,
		Ready:     []server.ReadyCheck{{Name: "database", Check: st.Ping}},
	})
	return a, nil
}

// Handler returns the root HTTP handler.
func (a *App) Handler() http.Handler { return a.handler }

// Store exposes the database, for the CLI and tests.
func (a *App) Store() *store.Store { return a.store }

// Close releases the database.
func (a *App) Close() error { return a.store.Close() }

// bootstrapAdmin creates the first dashboard user from FAULTBOX_ADMIN_EMAIL
// and FAULTBOX_ADMIN_PASSWORD, but only while no users exist (D9). Later
// changes to those variables are ignored, so they can't reset a password.
func (a *App) bootstrapAdmin(ctx context.Context) error {
	n, err := a.store.CountUsers(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if a.cfg.AdminEmail == "" {
		a.logger.Warn("no dashboard users exist; set FAULTBOX_ADMIN_EMAIL and FAULTBOX_ADMIN_PASSWORD or run 'faultbox user create'")
		return nil
	}
	if _, err := CreateUser(ctx, a.store, a.cfg.AdminEmail, a.cfg.AdminPassword, a.cfg.BcryptCost); err != nil {
		return fmt.Errorf("create bootstrap admin: %w", err)
	}
	a.logger.Info("created bootstrap admin user", "email", a.cfg.AdminEmail)
	return nil
}

// ErrUserExists is returned when the email is already registered.
var ErrUserExists = errors.New("a user with that email already exists")

// CreateUser validates and adds a dashboard user.
func CreateUser(ctx context.Context, st *store.Store, email, password string, cost int) (store.User, error) {
	email = strings.TrimSpace(email)
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return store.User{}, fmt.Errorf("%q is not a valid email address", email)
	}
	if len(password) < config.MinPasswordLength {
		return store.User{}, fmt.Errorf("password must be at least %d characters", config.MinPasswordLength)
	}
	hash, err := auth.HashPassword(password, cost)
	if err != nil {
		return store.User{}, err
	}
	u, err := st.CreateUser(ctx, email, hash)
	if errors.Is(err, store.ErrConflict) {
		return store.User{}, ErrUserExists
	}
	return u, err
}
