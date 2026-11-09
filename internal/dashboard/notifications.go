package dashboard

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/internal/notify"
	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// channelForm is the channel editor's state, re-rendered on validation
// errors. The SMTP password is never sent back to the browser.
type channelForm struct {
	ID              int64
	Kind            string
	Name            string
	Enabled         bool
	Project         string
	Environments    string
	OnNewIssue      bool
	OnRegression    bool
	OnQueueFailure  bool
	ThresholdCount  string
	ThresholdWindow string
	Cooldown        string
	// Email
	Host     string
	Port     string
	Security string
	Username string
	From     string
	To       string
	// Webhook
	URL string

	HasSecret bool
	Error     string
}

func newChannelForm(kind string) channelForm {
	f := channelForm{
		Kind: kind, Enabled: true, OnNewIssue: true, OnRegression: true, OnQueueFailure: true,
		ThresholdCount: "0", ThresholdWindow: "60", Cooldown: "30",
	}
	if kind == store.ChannelEmail {
		f.Port, f.Security = "587", store.SMTPStartTLS
	}
	return f
}

func formFromChannel(c store.Channel) channelForm {
	f := channelForm{
		ID: c.ID, Kind: c.Kind, Name: c.Name, Enabled: c.Enabled,
		Environments: strings.Join(c.Environments, ", "),
		OnNewIssue:   c.OnNewIssue, OnRegression: c.OnRegression, OnQueueFailure: c.OnQueueFailure,
		ThresholdCount: strconv.Itoa(c.ThresholdCount), ThresholdWindow: strconv.Itoa(c.ThresholdWindow),
		Cooldown: strconv.Itoa(c.Cooldown),
		Host:     c.Config.Host, Security: c.Config.Security, Username: c.Config.Username,
		From: c.Config.From, To: strings.Join(c.Config.To, ", "), URL: c.Config.URL,
		HasSecret: c.HasSecret(),
	}
	if c.ProjectID > 0 {
		f.Project = strconv.FormatInt(c.ProjectID, 10)
	}
	if c.Config.Port > 0 {
		f.Port = strconv.Itoa(c.Config.Port)
	}
	return f
}

func parseChannelForm(r *http.Request, kind string) channelForm {
	v := r.PostFormValue
	return channelForm{
		Kind: kind, Name: v("name"), Enabled: v("enabled") == "1",
		Project: v("project"), Environments: v("environments"),
		OnNewIssue: v("on_new_issue") == "1", OnRegression: v("on_regression") == "1",
		OnQueueFailure: v("on_queue_failure") == "1",
		ThresholdCount: strings.TrimSpace(v("threshold_count")), ThresholdWindow: strings.TrimSpace(v("threshold_window")),
		Cooldown: strings.TrimSpace(v("cooldown")),
		Host:     v("host"), Port: strings.TrimSpace(v("port")), Security: v("security"), Username: v("username"),
		From: v("from"), To: v("to"), URL: v("url"),
	}
}

// channel turns the form into a validated channel, without its secret.
func (f channelForm) channel() (store.Channel, error) {
	num := func(s, field string) (int, error) {
		if s == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, &store.ValidationError{Msg: field + " must be a whole number."}
		}
		return n, nil
	}
	c := store.Channel{
		ID: f.ID, Kind: f.Kind, Name: f.Name, Enabled: f.Enabled,
		OnNewIssue: f.OnNewIssue, OnRegression: f.OnRegression, OnQueueFailure: f.OnQueueFailure,
	}
	var err error
	if c.ThresholdCount, err = num(f.ThresholdCount, "The occurrence threshold"); err != nil {
		return c, err
	}
	if c.ThresholdWindow, err = num(f.ThresholdWindow, "The threshold window"); err != nil {
		return c, err
	}
	if c.Cooldown, err = num(f.Cooldown, "The cooldown"); err != nil {
		return c, err
	}
	if f.Project != "" {
		if c.ProjectID, err = strconv.ParseInt(f.Project, 10, 64); err != nil {
			return c, &store.ValidationError{Msg: "Choose a project from the list."}
		}
	}
	if c.Environments, err = store.NormalizeEnvironments(f.Environments); err != nil {
		return c, err
	}
	c.Config = store.ChannelConfig{Host: f.Host, Security: f.Security, Username: f.Username, From: f.From, URL: f.URL}
	if f.Kind == store.ChannelEmail {
		if c.Config.Port, err = num(f.Port, "The SMTP port"); err != nil {
			return c, err
		}
		c.Config.To = strings.FieldsFunc(f.To, func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == '\r' })
	}
	return c, store.NormalizeChannel(&c)
}

func (h *Handler) notifier() Notifier { return h.opts.Notifier }

// ---- list ----

func (h *Handler) notificationsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channels, err := h.store.Channels(ctx)
	if err != nil {
		h.serverError(w, err)
		return
	}
	names, projects, err := h.projectNames(r)
	if err != nil {
		h.serverError(w, err)
		return
	}
	status := r.URL.Query().Get("status")
	history, err := h.store.ListNotifications(ctx, store.NotificationFilter{Status: status, Limit: 50})
	if err != nil {
		h.serverError(w, err)
		return
	}
	stats, err := h.store.NotificationStats(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		h.serverError(w, err)
		return
	}
	enabled := 0
	for _, c := range channels {
		if c.Enabled {
			enabled++
		}
	}
	pg := h.newPage(r, "Alerts", "notifications", map[string]any{
		"Channels": channels,
		"Names":    names,
		"Projects": projects,
		"History":  history,
		"Status":   status,
		"Stats":    stats,
		"Enabled":  enabled,
	})
	pg.Flash = notificationFlash(r.URL.Query().Get("done"))
	h.render(w, http.StatusOK, "notifications", "", pg)
}

func notificationFlash(done string) string {
	switch done {
	case "saved":
		return "Channel saved."
	case "deleted":
		return "Channel deleted."
	case "test":
		return "Test notification queued. Its delivery appears in the history below."
	case "retry":
		return "Delivery queued again."
	case "on":
		return "Channel turned on."
	case "off":
		return "Channel turned off. It won't send alerts until you turn it back on."
	}
	return ""
}

// ---- create and edit ----

func (h *Handler) renderChannelForm(w http.ResponseWriter, r *http.Request, status int, form channelForm) {
	_, projects, err := h.projectNames(r)
	if err != nil {
		h.serverError(w, err)
		return
	}
	title := "New " + form.Kind + " channel"
	var history []store.Notification
	if form.ID > 0 {
		title = form.Name
		if history, err = h.store.ListNotifications(r.Context(), store.NotificationFilter{ChannelID: form.ID, Limit: 20}); err != nil {
			h.serverError(w, err)
			return
		}
	}
	pg := h.newPage(r, title, "notifications", map[string]any{
		"Form": form, "Projects": projects, "History": history,
	})
	pg.Flash = notificationFlash(r.URL.Query().Get("done"))
	h.render(w, status, "notification_channel", "", pg)
}

func (h *Handler) channelNew(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind != store.ChannelEmail && kind != store.ChannelWebhook {
		kind = store.ChannelWebhook
	}
	h.renderChannelForm(w, r, http.StatusOK, newChannelForm(kind))
}

func (h *Handler) channelCreate(w http.ResponseWriter, r *http.Request) {
	kind := r.PostFormValue("kind")
	if kind != store.ChannelEmail && kind != store.ChannelWebhook {
		h.errorPage(w, r, http.StatusBadRequest, "Unknown channel type.")
		return
	}
	form := parseChannelForm(r, kind)
	c, err := form.channel()
	if msg, ok := validationMessage(err); ok {
		form.Error = msg
		h.renderChannelForm(w, r, http.StatusUnprocessableEntity, form)
		return
	}
	if err != nil {
		h.serverError(w, err)
		return
	}

	// Email: the SMTP password, if any. Webhook: a generated signing secret,
	// shown once after creation.
	var plain string
	switch kind {
	case store.ChannelEmail:
		plain = r.PostFormValue("password")
	case store.ChannelWebhook:
		if plain, err = notify.NewWebhookSecret(); err != nil {
			h.serverError(w, err)
			return
		}
	}
	if plain != "" {
		if c.Secret, err = auth.Seal(h.opts.EncryptionKey, []byte(plain)); err != nil {
			h.serverError(w, err)
			return
		}
	}
	id, err := h.store.CreateChannel(r.Context(), c)
	if err != nil {
		h.serverError(w, err)
		return
	}
	h.notifier().InvalidateChannels()
	h.logger.Info("notification channel created", "channel_id", id, "kind", kind, "user_id", h.userID(r))
	if kind == store.ChannelWebhook {
		c.ID = id
		h.renderSecret(w, r, c, plain, true)
		return
	}
	http.Redirect(w, r, "/notifications?done=saved", http.StatusSeeOther)
}

func (h *Handler) renderSecret(w http.ResponseWriter, r *http.Request, c store.Channel, secret string, created bool) {
	h.render(w, http.StatusOK, "notification_secret", "", h.newPage(r, c.Name, "notifications", map[string]any{
		"Channel": c, "Secret": secret, "Created": created,
		"SignatureHeader": notify.SignatureHeader, "TimestampHeader": notify.TimestampHeader,
	}))
}

func (h *Handler) loadChannel(w http.ResponseWriter, r *http.Request) (store.Channel, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		h.notFound(w, r)
		return store.Channel{}, false
	}
	c, err := h.store.Channel(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r)
		return store.Channel{}, false
	}
	if err != nil {
		h.serverError(w, err)
		return store.Channel{}, false
	}
	return c, true
}

func (h *Handler) channelShow(w http.ResponseWriter, r *http.Request) {
	c, ok := h.loadChannel(w, r)
	if !ok {
		return
	}
	h.renderChannelForm(w, r, http.StatusOK, formFromChannel(c))
}

func (h *Handler) channelUpdate(w http.ResponseWriter, r *http.Request) {
	old, ok := h.loadChannel(w, r)
	if !ok {
		return
	}
	form := parseChannelForm(r, old.Kind)
	form.ID, form.HasSecret = old.ID, old.HasSecret()
	c, err := form.channel()
	if msg, ok := validationMessage(err); ok {
		form.Error = msg
		h.renderChannelForm(w, r, http.StatusUnprocessableEntity, form)
		return
	}
	if err != nil {
		h.serverError(w, err)
		return
	}
	// A blank password keeps the stored one; "clear_password" removes it.
	replace := false
	if old.Kind == store.ChannelEmail {
		if pw := r.PostFormValue("password"); pw != "" {
			if c.Secret, err = auth.Seal(h.opts.EncryptionKey, []byte(pw)); err != nil {
				h.serverError(w, err)
				return
			}
			replace = true
		} else if r.PostFormValue("clear_password") == "1" {
			replace = true
		}
	}
	if err := h.store.UpdateChannel(r.Context(), c, replace); err != nil {
		h.serverError(w, err)
		return
	}
	h.notifier().InvalidateChannels()
	http.Redirect(w, r, "/notifications/channels/"+strconv.FormatInt(c.ID, 10)+"?done=saved", http.StatusSeeOther)
}

func (h *Handler) channelSetEnabled(w http.ResponseWriter, r *http.Request) {
	c, ok := h.loadChannel(w, r)
	if !ok {
		return
	}
	on := r.PostFormValue("enabled") == "1"
	if err := h.store.SetChannelEnabled(r.Context(), c.ID, on); err != nil {
		h.serverError(w, err)
		return
	}
	h.notifier().InvalidateChannels()
	done := "off"
	if on {
		done = "on"
	}
	http.Redirect(w, r, "/notifications?done="+done, http.StatusSeeOther)
}

func (h *Handler) channelDelete(w http.ResponseWriter, r *http.Request) {
	c, ok := h.loadChannel(w, r)
	if !ok {
		return
	}
	if err := h.store.DeleteChannel(r.Context(), c.ID); err != nil {
		h.serverError(w, err)
		return
	}
	h.notifier().InvalidateChannels()
	h.logger.Info("notification channel deleted", "channel_id", c.ID, "user_id", h.userID(r))
	http.Redirect(w, r, "/notifications?done=deleted", http.StatusSeeOther)
}

func (h *Handler) channelTest(w http.ResponseWriter, r *http.Request) {
	c, ok := h.loadChannel(w, r)
	if !ok {
		return
	}
	if _, err := h.notifier().SendTest(r.Context(), c.ID); err != nil {
		h.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/notifications/channels/"+strconv.FormatInt(c.ID, 10)+"?done=test", http.StatusSeeOther)
}

func (h *Handler) channelRotateSecret(w http.ResponseWriter, r *http.Request) {
	c, ok := h.loadChannel(w, r)
	if !ok {
		return
	}
	if c.Kind != store.ChannelWebhook {
		h.errorPage(w, r, http.StatusBadRequest, "Only webhook channels have a signing secret.")
		return
	}
	secret, err := notify.NewWebhookSecret()
	if err != nil {
		h.serverError(w, err)
		return
	}
	if c.Secret, err = auth.Seal(h.opts.EncryptionKey, []byte(secret)); err != nil {
		h.serverError(w, err)
		return
	}
	if err := h.store.UpdateChannel(r.Context(), c, true); err != nil {
		h.serverError(w, err)
		return
	}
	h.logger.Info("webhook secret rotated", "channel_id", c.ID, "user_id", h.userID(r))
	h.renderSecret(w, r, c, secret, false)
}

func (h *Handler) notificationRetry(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		h.notFound(w, r)
		return
	}
	if err := h.notifier().Retry(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		h.errorPage(w, r, http.StatusConflict, "Only failed deliveries can be retried.")
		return
	} else if err != nil {
		h.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/notifications?done=retry", http.StatusSeeOther)
}
