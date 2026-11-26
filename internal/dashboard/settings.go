package dashboard

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// retention returns the policy in force: Settings page values over the
// environment's.
func (h *Handler) retention(ctx context.Context) (store.Retention, error) {
	return h.store.Retention(ctx, h.opts.ShortRetentionHours, h.opts.RetentionDays)
}

// retentionForm is the Settings page's retention section.
type retentionForm struct {
	ShortHours string // as entered, so a rejected value is shown again
	IssueDays  string
	Error      string
	// Impact lists what saving would delete at the next sweep. When it is
	// set, the page asks for confirmation before saving.
	Impact      []store.PrunedCount
	ImpactTotal int64
}

// ShortOptions are the short-lived retention choices: 24 and 48 hours, and
// the values in use or from the environment if they differ.
func (f retentionForm) ShortOptions(policy store.Retention, envHours int) []int {
	opts := []int{24, 48}
	for _, v := range []int{policy.ShortHours, envHours} {
		if v >= store.MinShortRetentionHours && v <= store.MaxShortRetentionHours && !slices.Contains(opts, v) {
			opts = append(opts, v)
		}
	}
	slices.Sort(opts)
	return opts
}

// Selected reports whether hours is the form's short-lived value.
func (f retentionForm) Selected(hours int) bool { return f.ShortHours == strconv.Itoa(hours) }

func (h *Handler) settings(w http.ResponseWriter, r *http.Request) {
	pg, ok := h.settingsPage(w, r, nil)
	if !ok {
		return
	}
	switch r.URL.Query().Get("saved") {
	case "retention":
		pg.Flash = "Retention saved. It applies from the next sweep, within 10 minutes."
	case "reset":
		pg.Flash = "Retention reset to the environment's values."
	}
	h.render(w, http.StatusOK, "settings", "", pg)
}

// settingsPage loads the Settings page. form is the retention section as
// submitted, or nil for the saved values. It writes the error response
// itself and returns false.
func (h *Handler) settingsPage(w http.ResponseWriter, r *http.Request, form *retentionForm) (page, bool) {
	ctx := r.Context()
	version, err := h.store.SchemaVersion(ctx)
	if err != nil {
		h.serverError(w, err)
		return page{}, false
	}
	users, err := h.store.CountUsers(ctx)
	if err != nil {
		h.serverError(w, err)
		return page{}, false
	}
	policy, err := h.retention(ctx)
	if err != nil {
		h.serverError(w, err)
		return page{}, false
	}
	sizes, err := h.store.PrunedTableSizes(ctx)
	if err != nil {
		h.serverError(w, err)
		return page{}, false
	}
	if form == nil {
		form = &retentionForm{ShortHours: strconv.Itoa(policy.ShortHours), IssueDays: strconv.Itoa(policy.IssueDays)}
	}
	envShort, envDays := h.envRetention()
	return h.newPage(r, "Settings", "settings", map[string]any{
		"SchemaVersion": version,
		"DatabasePath":  h.store.Path(),
		"DatabaseSize":  databaseSize(h.store.Path()),
		"Retention":     policy,
		"EnvShortHours": envShort,
		"EnvIssueDays":  envDays,
		"Form":          form,
		"Sizes":         sizes,
		"Users":         users,
		"Ingest":        h.metricsSnapshot(),
	}), true
}

// envRetention is the environment's retention, with the defaults applied.
func (h *Handler) envRetention() (shortHours, days int) {
	shortHours, days = h.opts.ShortRetentionHours, h.opts.RetentionDays
	if shortHours < store.MinShortRetentionHours || shortHours > store.MaxShortRetentionHours {
		shortHours = store.DefaultShortRetentionHours
	}
	if days < store.MinIssueRetentionDays || days > store.MaxIssueRetentionDays {
		days = store.DefaultIssueRetentionDays
	}
	return shortHours, days
}

// settingsRetention saves the retention section. A change that would
// delete data is shown with an estimate first and saved only once
// confirmed; reset forgets the saved values.
func (h *Handler) settingsRetention(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "The form could not be read.")
		return
	}
	ctx := r.Context()
	if r.PostFormValue("reset") == "1" {
		if err := h.store.ResetRetention(ctx); err != nil {
			h.serverError(w, err)
			return
		}
		http.Redirect(w, r, "/settings?saved=reset", http.StatusSeeOther)
		return
	}

	form := &retentionForm{
		ShortHours: strings.TrimSpace(r.PostFormValue("short_hours")),
		IssueDays:  strings.TrimSpace(r.PostFormValue("issue_days")),
	}
	fail := func(message string) {
		form.Error = message
		if pg, ok := h.settingsPage(w, r, form); ok {
			h.render(w, http.StatusUnprocessableEntity, "settings", "", pg)
		}
	}
	short, errShort := strconv.Atoi(form.ShortHours)
	days, errDays := strconv.Atoi(form.IssueDays)
	if errShort != nil || errDays != nil {
		fail("Enter whole numbers: hours for short-lived data, days for issues and history.")
		return
	}
	if err := store.ValidateRetention(short, days); err != nil {
		msg := err.Error()
		if errors.Is(err, store.ErrRetentionInvalid) {
			msg = strings.TrimPrefix(msg, store.ErrRetentionInvalid.Error()+": ")
		}
		fail(strings.ToUpper(msg[:1]) + msg[1:] + ".")
		return
	}

	if r.PostFormValue("confirm") != "1" {
		impact, err := h.store.RetentionImpact(ctx, store.Retention{ShortHours: short, IssueDays: days})
		if err != nil {
			h.serverError(w, err)
			return
		}
		// Only what the change itself adds: rows the current policy would
		// delete anyway at the next sweep don't need a warning.
		current, err := h.retention(ctx)
		if err != nil {
			h.serverError(w, err)
			return
		}
		before, err := h.store.RetentionImpact(ctx, current)
		if err != nil {
			h.serverError(w, err)
			return
		}
		form.Impact, form.ImpactTotal = impactDiff(impact, before)
		if form.ImpactTotal > 0 {
			pg, ok := h.settingsPage(w, r, form)
			if ok {
				h.render(w, http.StatusOK, "settings", "", pg)
			}
			return
		}
	}
	if err := h.store.SaveRetention(ctx, short, days); err != nil {
		h.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/settings?saved=retention", http.StatusSeeOther)
}

// impactDiff subtracts what the current policy deletes anyway.
func impactDiff(next, current []store.PrunedCount) ([]store.PrunedCount, int64) {
	already := map[string]int64{}
	for _, c := range current {
		already[c.Label] = c.Rows
	}
	var (
		out   []store.PrunedCount
		total int64
	)
	for _, c := range next {
		if n := c.Rows - already[c.Label]; n > 0 {
			out = append(out, store.PrunedCount{Label: c.Label, Rows: n})
			total += n
		}
	}
	return out, total
}
