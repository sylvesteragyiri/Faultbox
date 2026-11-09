// Package httpx holds HTTP helpers shared by the API and the dashboard.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// ErrorBody is the JSON error envelope used by every API response.
//
//	{"error": {"code": "invalid_api_key", "message": "..."}}
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is a machine-readable code plus a human-readable message.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes the standard JSON error envelope.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, ErrorBody{Error: ErrorDetail{Code: code, Message: message}})
}

// JSONMuxErrors rewrites the plain-text 404 and 405 replies produced by
// http.ServeMux into the JSON error envelope, so API clients always get JSON.
func JSONMuxErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&muxErrorWriter{ResponseWriter: w}, r)
	})
}

type muxErrorWriter struct {
	http.ResponseWriter
	swallow bool
}

func (m *muxErrorWriter) WriteHeader(status int) {
	plain := strings.HasPrefix(m.Header().Get("Content-Type"), "text/plain")
	if plain && (status == http.StatusNotFound || status == http.StatusMethodNotAllowed) {
		m.swallow = true
		m.Header().Del("X-Content-Type-Options")
		code, msg := "not_found", "No such endpoint."
		if status == http.StatusMethodNotAllowed {
			code, msg = "method_not_allowed", "Method not allowed for this endpoint."
		}
		WriteError(m.ResponseWriter, status, code, msg)
		return
	}
	m.ResponseWriter.WriteHeader(status)
}

func (m *muxErrorWriter) Write(b []byte) (int, error) {
	if m.swallow {
		return len(b), nil
	}
	return m.ResponseWriter.Write(b)
}

func (m *muxErrorWriter) Unwrap() http.ResponseWriter { return m.ResponseWriter }

// ClientIP returns the caller's IP. X-Forwarded-For is only trusted when
// trustProxy is set, because clients can send any value they like.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := strings.TrimSpace(first); net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// IsHTTPS reports whether the client connected over HTTPS.
func IsHTTPS(r *http.Request, trustProxy bool) bool {
	if r.TLS != nil {
		return true
	}
	return trustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// SecurityHeaders sets defensive headers on every response. The CSP allows
// only same-origin scripts and styles, so templates must not use inline
// <script> or style attributes.
func SecurityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
		"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// Recover turns a panic into a 500 response and logs the stack.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared as documented by net/http
						panic(v)
					}
					logger.Error("panic serving request", "method", r.Method, "path", r.URL.Path,
						"panic", v, "stack", string(debug.Stack()))
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// LogRequests logs one line per request. Health probes log at debug level.
func LogRequests(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			level := slog.LevelInfo
			if r.URL.Path == "/health" || r.URL.Path == "/ready" || strings.HasPrefix(r.URL.Path, "/static/") {
				level = slog.LevelDebug
			}
			logger.Log(r.Context(), level, "request",
				"method", r.Method, "path", r.URL.Path, "status", rec.status,
				"duration_ms", time.Since(start).Milliseconds())
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(status int) {
	if !s.wroteHeader {
		s.status, s.wroteHeader = status, true
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
