// Command faultbox runs the Faultbox server and its admin commands.
//
//	faultbox                                   run the server
//	faultbox healthcheck                       probe /ready (Docker HEALTHCHECK)
//	faultbox user create --email you@x.com     add a dashboard user; password on stdin
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/app"
	"github.com/sylvesteragyiri/faultbox/internal/config"
	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	args := os.Args[1:]
	switch {
	case len(args) == 0 || args[0] == "serve":
		os.Exit(serve())
	case args[0] == "healthcheck":
		os.Exit(healthcheck())
	case args[0] == "user" && len(args) > 1 && args[1] == "create":
		os.Exit(userCreate(args[2:], os.Stdin))
	case args[0] == "version" || args[0] == "--version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\nusage: faultbox [serve | healthcheck | user create --email EMAIL | version]\n", strings.Join(args, " "))
		os.Exit(2)
	}
}

func newLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

func serve() int {
	cfg, err := config.Load(os.Getenv)
	logger := newLogger(cfg.LogLevel)
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		return 1
	}
	if err := run(cfg, logger); err != nil {
		logger.Error("server exited", "error", err)
		return 1
	}
	return 0
}

func run(cfg config.Config, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, cfg, logger, version)
	if err != nil {
		return err
	}
	defer func() {
		if err := a.Close(); err != nil {
			logger.Error("close database", "error", err)
		}
	}()

	addr := net.JoinHostPort("", cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           a.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr, "version", version, "database", cfg.DatabasePath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down", "timeout", cfg.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// healthcheck probes /ready on the local server. It exists because the
// distroless runtime image has no curl or wget for Docker's HEALTHCHECK.
func healthcheck() int {
	port := os.Getenv("FAULTBOX_PORT")
	if port == "" {
		port = "8080"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		fmt.Fprintf(os.Stderr, "invalid FAULTBOX_PORT %q\n", port)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Loopback only, and port is validated above.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/ready", nil) //nolint:gosec // G704: fixed loopback URL
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: fixed loopback URL
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "not ready:", resp.Status)
		return 1
	}
	return 0
}

// userCreate adds a dashboard user. The password is read from the first line
// of stdin so it never appears in shell history or the process list.
func userCreate(args []string, stdin io.Reader) int {
	fs := flag.NewFlagSet("user create", flag.ContinueOnError)
	email := fs.String("email", "", "email address of the new user")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *email == "" {
		fmt.Fprintln(os.Stderr, "usage: echo 'password' | faultbox user create --email you@example.com")
		return 2
	}

	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, "read password:", err)
		return 1
	}
	password := strings.TrimRight(line, "\r\n")

	path := os.Getenv("FAULTBOX_DATABASE_PATH")
	if path == "" {
		path = "faultbox.db"
	}
	ctx := context.Background()
	st, err := store.Open(ctx, path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = st.Close() }()

	u, err := app.CreateUser(ctx, st, *email, password, 12)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create user:", err)
		return 1
	}
	fmt.Printf("created user %s (id %d)\n", u.Email, u.ID)
	return 0
}
