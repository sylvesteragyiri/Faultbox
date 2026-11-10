package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/internal/httpx"
	"github.com/sylvesteragyiri/faultbox/internal/store"
)

const (
	sessionCookie = "faultbox_session"
	csrfCookie    = "faultbox_csrf"
	csrfHeader    = "X-CSRF-Token"
	csrfField     = "csrf_token"

	// Sliding expiry is refreshed at most this often, to avoid a database
	// write on every page view.
	sessionRefreshEvery = time.Hour
)

func (h *Handler) secureCookies(r *http.Request) bool {
	return httpx.IsHTTPS(r, h.opts.TrustProxy)
}

// withCSRF issues a random per-browser cookie and requires every unsafe
// request to echo an HMAC of it, in the X-CSRF-Token header (HTMX) or the
// csrf_token form field (plain forms). An attacker's page can neither read
// the cookie nor compute the HMAC without FAULTBOX_SECRET.
func (h *Handler) withCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var cookieValue string
		if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) >= 32 {
			cookieValue = c.Value
		} else {
			buf := make([]byte, 32)
			_, _ = rand.Read(buf)
			cookieValue = base64.RawURLEncoding.EncodeToString(buf)
			// Secure is set whenever the request arrived over HTTPS; plain
			// HTTP is allowed for local and private-network installs.
			http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is conditional on HTTPS, see above
				Name: csrfCookie, Value: cookieValue, Path: "/",
				HttpOnly: true, Secure: h.secureCookies(r), SameSite: http.SameSiteLaxMode,
			})
		}

		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			token := r.Header.Get(csrfHeader)
			if token == "" {
				token = r.PostFormValue(csrfField)
			}
			if !auth.ValidCSRFToken(h.opts.CSRFKey, cookieValue, token) {
				h.errorPage(w, r, http.StatusForbidden,
					"Your form expired or came from another site. Go back, reload the page and try again.")
				return
			}
		}

		ctx := context.WithValue(r.Context(), csrfKey, auth.CSRFToken(h.opts.CSRFKey, cookieValue))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// withSession loads the logged-in user, if any, into the request context.
func (h *Handler) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		sess, user, err := h.store.SessionUser(r.Context(), auth.HashToken(c.Value))
		if errors.Is(err, store.ErrNotFound) {
			h.clearSessionCookie(w, r)
			next.ServeHTTP(w, r)
			return
		}
		if err != nil {
			h.serverError(w, err)
			return
		}
		if time.Since(sess.LastSeenAt) > sessionRefreshEvery {
			if err := h.store.ExtendSession(r.Context(), sess.ID, h.opts.SessionTTL); err != nil {
				h.logger.Warn("extend session", "error", err)
			} else {
				h.setSessionCookie(w, r, c.Value)
			}
		}
		ctx := context.WithValue(r.Context(), userKey, user)
		ctx = context.WithValue(ctx, sessionKey, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireUser sends anonymous visitors to the login page.
func (h *Handler) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := userFrom(r.Context()); ok {
			next.ServeHTTP(w, r)
			return
		}
		target := "/login"
		if r.Method == http.MethodGet && r.URL.Path != "/" {
			target += "?next=" + url.QueryEscape(r.URL.RequestURI())
		}
		if isHTMX(r) {
			w.Header().Set("HX-Redirect", target)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
	})
}

func (h *Handler) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is conditional on HTTPS (secureCookies)
		Name: sessionCookie, Value: token, Path: "/",
		MaxAge:   int(h.opts.SessionTTL / time.Second),
		HttpOnly: true, Secure: h.secureCookies(r), SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is conditional on HTTPS (secureCookies)
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: h.secureCookies(r), SameSite: http.SameSiteLaxMode,
	})
}

// safeNext only allows same-site relative paths, so ?next= can't be used to
// redirect users to another site after login.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") {
		return "/"
	}
	return next
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := userFrom(r.Context()); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	h.renderLogin(w, r, http.StatusOK, "", "")
}

func (h *Handler) renderLogin(w http.ResponseWriter, r *http.Request, status int, email, errMsg string) {
	users, err := h.store.CountUsers(r.Context())
	if err != nil {
		h.serverError(w, err)
		return
	}
	h.render(w, status, "login", "", h.newPage(r, "Log in", "", map[string]any{
		"Email":            email,
		"Error":            errMsg,
		"Next":             safeNext(r.FormValue("next")),
		"NoUsers":          users == 0,
		"TurnstileSiteKey": h.opts.TurnstileSiteKey,
	}))
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	ipKey := "ip:" + h.clientIP(r)
	emailKey := "email:" + strings.ToLower(email)

	if h.loginByIP.Blocked(ipKey) || h.loginByID.Blocked(emailKey) {
		h.renderLogin(w, r, http.StatusTooManyRequests, email,
			"Too many failed attempts. Wait 15 minutes and try again.")
		return
	}
	if h.opts.TurnstileSiteKey != "" {
		valid, err := h.verifyTurnstile(r.Context(), r.PostFormValue("cf-turnstile-response"), requestHostname(r.Host))
		if err != nil {
			h.logger.Warn("turnstile verification unavailable", "error", err)
			h.renderLogin(w, r, http.StatusServiceUnavailable, email, "The security check is unavailable. Try again shortly.")
			return
		}
		if !valid {
			h.loginByIP.Fail(ipKey)
			h.loginByID.Fail(emailKey)
			h.renderLogin(w, r, http.StatusUnauthorized, email, "Complete the security check and try again.")
			return
		}
	}

	user, hash, err := h.store.UserByEmail(r.Context(), email)
	switch {
	case errors.Is(err, store.ErrNotFound):
		auth.EqualiseTiming(password)
	case err != nil:
		h.serverError(w, err)
		return
	}
	if err != nil || !auth.CheckPassword(hash, password) {
		h.loginByIP.Fail(ipKey)
		h.loginByID.Fail(emailKey)
		h.logger.Warn("failed login", "ip", h.clientIP(r))
		h.renderLogin(w, r, http.StatusUnauthorized, email, "That email and password don't match.")
		return
	}

	token, tokenHash, err := auth.NewToken()
	if err != nil {
		h.serverError(w, err)
		return
	}
	if _, err := h.store.CreateSession(r.Context(), user.ID, tokenHash, h.opts.SessionTTL); err != nil {
		h.serverError(w, err)
		return
	}
	h.loginByIP.Reset(ipKey)
	h.loginByID.Reset(emailKey)
	h.setSessionCookie(w, r, token)
	h.logger.Info("login", "user_id", user.ID)
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther) //nolint:gosec // G710: safeNext allows only local paths
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := h.store.DeleteSession(r.Context(), auth.HashToken(c.Value)); err != nil {
			h.serverError(w, err)
			return
		}
	}
	h.clearSessionCookie(w, r)
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
