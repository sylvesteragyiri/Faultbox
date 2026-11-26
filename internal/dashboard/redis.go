package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/internal/redismon"
	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// redisForm holds the values a Redis form re-renders with after a failed
// save. The endpoint is deliberately absent: a URL may carry a password and
// is never echoed back to the browser.
type redisForm struct {
	Name         string
	Enabled      bool
	AlertEnabled bool
	Error        string
}

// redisState is the one-word health shown beside a server.
type redisState struct {
	Label string
	Badge string
}

func redisStateOf(t store.RedisTarget) redisState {
	switch {
	case !t.Enabled:
		return redisState{Label: "Paused", Badge: "badge"}
	case t.LastError != "":
		return redisState{Label: "Failing", Badge: "badge badge-off"}
	case t.LastChecked.IsZero():
		return redisState{Label: "Waiting", Badge: "badge badge-info"}
	}
	return redisState{Label: "Healthy", Badge: "badge badge-ok"}
}

type redisRow struct {
	store.RedisStatus
	State redisState
}

type redisSummary struct {
	Total, Healthy, Failing, Paused int
	UsedMemory                      int64
}

func (h *Handler) redisPage(w http.ResponseWriter, r *http.Request) {
	statuses, err := h.store.RedisStatuses(r.Context())
	if err != nil {
		h.serverError(w, err)
		return
	}
	rows := make([]redisRow, 0, len(statuses))
	var sum redisSummary
	for _, s := range statuses {
		row := redisRow{RedisStatus: s, State: redisStateOf(s.Target)}
		rows = append(rows, row)
		sum.Total++
		switch row.State.Label {
		case "Healthy":
			sum.Healthy++
		case "Failing":
			sum.Failing++
		case "Paused":
			sum.Paused++
		}
		if s.Latest != nil && s.Target.Enabled {
			sum.UsedMemory += s.Latest.UsedMemory
		}
	}
	pg := h.newPage(r, "Redis", "redis", map[string]any{
		"Rows": rows, "Summary": sum, "Thresholds": redismon.AlertThresholds,
	})
	if r.URL.Query().Get("done") == "deleted" {
		pg.Flash = "Redis server and its samples deleted."
	}
	h.render(w, http.StatusOK, "redis", "", pg)
}

func (h *Handler) redisNew(w http.ResponseWriter, r *http.Request) {
	h.renderRedisNew(w, r, http.StatusOK, redisForm{Enabled: true})
}

func (h *Handler) renderRedisNew(w http.ResponseWriter, r *http.Request, status int, form redisForm) {
	h.render(w, status, "redis_new", "", h.newPage(r, "Add Redis server", "redis", map[string]any{
		"Form": form, "Thresholds": redismon.AlertThresholds,
	}))
}

func (h *Handler) redisTestNew(w http.ResponseWriter, r *http.Request) {
	h.redisConnectionTest(w, r, 0)
}

func (h *Handler) redisTestExisting(w http.ResponseWriter, r *http.Request) {
	id, ok := redisPathID(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	h.redisConnectionTest(w, r, id)
}

func (h *Handler) redisConnectionTest(w http.ResponseWriter, r *http.Request, targetID int64) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "The form could not be read.")
		return
	}
	endpoint := strings.TrimSpace(r.PostFormValue("endpoint"))
	backURL := "/redis/new"
	targetName := strings.TrimSpace(r.PostFormValue("name"))
	usedSaved := false
	if targetID > 0 {
		backURL = "/redis/" + strconv.FormatInt(targetID, 10) + "/edit"
		target, err := h.store.RedisTarget(r.Context(), targetID)
		if errors.Is(err, store.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		if err != nil {
			h.serverError(w, err)
			return
		}
		if targetName == "" {
			targetName = target.Name
		}
		if endpoint == "" {
			plain, err := auth.Unseal(h.opts.EncryptionKey, target.Endpoint)
			if err != nil {
				h.redisTestResult(w, r, backURL, targetName, false, store.RedisSample{},
					"The saved Redis URL could not be decrypted. Enter the URL again and save it.")
				return
			}
			endpoint, usedSaved = string(plain), true
		}
	}
	if endpoint == "" {
		h.redisTestResult(w, r, backURL, targetName, false, store.RedisSample{}, "Enter a Redis URL to test the connection.")
		return
	}
	if err := checkRedisEndpoint(endpoint); err != nil {
		h.redisTestResult(w, r, backURL, targetName, usedSaved, store.RedisSample{}, redismon.Reason(err))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	sample, err := redismon.ReadInfo(ctx, endpoint)
	h.redisTestResult(w, r, backURL, targetName, usedSaved, sample, redismon.Reason(err))
}

func (h *Handler) redisTestResult(w http.ResponseWriter, r *http.Request, backURL, targetName string, usedSaved bool, sample store.RedisSample, failure string) {
	pg := h.newPage(r, "Redis connection test", "redis", map[string]any{
		"BackURL": backURL, "TargetName": targetName, "Sample": sample, "Failure": failure, "UsedSaved": usedSaved,
	})
	if isHTMX(r) {
		h.render(w, http.StatusOK, "redis_test", "result", pg)
		return
	}
	h.render(w, http.StatusOK, "redis_test", "", pg)
}

func (h *Handler) redisTargetPage(w http.ResponseWriter, r *http.Request) {
	id, ok := redisPathID(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	pg, ok := h.redisTargetData(w, r, id)
	if !ok {
		return
	}
	if r.URL.Query().Get("saved") == "1" {
		pg.Flash = "Redis server saved."
	}
	h.render(w, http.StatusOK, "redis_target", "", pg)
}

// redisTargetData loads everything the server page shows. It writes the
// error response itself and returns false.
func (h *Handler) redisTargetData(w http.ResponseWriter, r *http.Request, id int64) (page, bool) {
	ctx := r.Context()
	target, ok := h.redisTargetOr404(w, r, id)
	if !ok {
		return page{}, false
	}
	var latest *store.RedisSample
	if sample, sampleErr := h.store.LatestRedisSample(ctx, id); sampleErr == nil {
		latest = &sample
	} else if !errors.Is(sampleErr, store.ErrNotFound) {
		h.serverError(w, sampleErr)
		return page{}, false
	}
	policy, err := h.retention(ctx)
	if err != nil {
		h.serverError(w, err)
		return page{}, false
	}
	history, err := h.store.RedisHistory(ctx, id, time.Now().AddDate(0, 0, -policy.IssueDays))
	if err != nil {
		h.serverError(w, err)
		return page{}, false
	}
	return h.newPage(r, target.Name, "redis", map[string]any{
		"Target": target, "State": redisStateOf(target), "Latest": latest, "Plot": makeRedisPlot(history),
		"RetentionDays": policy.IssueDays,
	}), true
}

func (h *Handler) redisTargetOr404(w http.ResponseWriter, r *http.Request, id int64) (store.RedisTarget, bool) {
	target, err := h.store.RedisTarget(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r)
		return target, false
	}
	if err != nil {
		h.serverError(w, err)
		return target, false
	}
	return target, true
}

// redisEdit shows a server's settings and delete action.
func (h *Handler) redisEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := redisPathID(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	h.renderRedisEdit(w, r, http.StatusOK, id, nil)
}

// renderRedisEdit renders the edit page. When form is not nil, it shows
// those values and their error instead of the saved settings.
func (h *Handler) renderRedisEdit(w http.ResponseWriter, r *http.Request, status int, id int64, form *redisForm) {
	target, ok := h.redisTargetOr404(w, r, id)
	if !ok {
		return
	}
	f := redisForm{Name: target.Name, Enabled: target.Enabled, AlertEnabled: target.AlertEnabled}
	if form != nil {
		f = *form
	}
	h.render(w, status, "redis_edit", "", h.newPage(r, "Edit "+target.Name, "redis", map[string]any{
		"Target": target, "Form": f, "Thresholds": redismon.AlertThresholds,
	}))
}

func (h *Handler) redisSave(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "The form could not be read.")
		return
	}
	id := int64(0)
	if r.PathValue("id") != "" {
		var ok bool
		if id, ok = redisPathID(r); !ok {
			h.notFound(w, r)
			return
		}
	}
	form := redisForm{
		Name:         strings.TrimSpace(r.PostFormValue("name")),
		Enabled:      r.PostFormValue("enabled") == "1",
		AlertEnabled: r.PostFormValue("alert_enabled") == "1",
	}
	fail := func(status int, message string) {
		form.Error = message
		if id == 0 {
			h.renderRedisNew(w, r, status, form)
			return
		}
		h.renderRedisEdit(w, r, status, id, &form)
	}
	name, err := store.NormalizeRedisName(form.Name)
	if err != nil {
		fail(http.StatusUnprocessableEntity, "Server name is required and must be at most 100 characters.")
		return
	}
	form.Name = name
	endpoint := strings.TrimSpace(r.PostFormValue("endpoint"))
	var sealed []byte
	if endpoint != "" {
		if err := checkRedisEndpoint(endpoint); err != nil {
			fail(http.StatusUnprocessableEntity, redismon.Reason(err))
			return
		}
		sealed, err = auth.Seal(h.opts.EncryptionKey, []byte(endpoint))
		if err != nil {
			h.serverError(w, err)
			return
		}
	} else if id == 0 {
		fail(http.StatusUnprocessableEntity, "Redis URL is required.")
		return
	}
	savedID, err := h.store.SaveRedisTarget(r.Context(), store.RedisTarget{
		ID: id, Name: name, Endpoint: sealed, Enabled: form.Enabled, AlertEnabled: form.AlertEnabled,
	})
	switch {
	case errors.Is(err, store.ErrConflict):
		fail(http.StatusConflict, "A Redis server with that name already exists.")
		return
	case errors.Is(err, store.ErrNotFound):
		h.notFound(w, r)
		return
	case err != nil:
		h.serverError(w, err)
		return
	}
	if id == 0 {
		id = savedID
	}
	h.logger.Info("Redis monitoring target saved", "target_id", id, "user_id", h.userID(r))
	http.Redirect(w, r, "/redis/"+strconv.FormatInt(id, 10)+"?saved=1", http.StatusSeeOther)
}

func (h *Handler) redisDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := redisPathID(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	if err := h.store.DeleteRedisTarget(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		h.serverError(w, err)
		return
	}
	h.logger.Info("Redis monitoring target deleted", "target_id", id, "user_id", h.userID(r))
	http.Redirect(w, r, "/redis?done=deleted", http.StatusSeeOther)
}

func redisPathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// checkRedisEndpoint applies the size bound and URL rules shared by saving
// and testing a server.
func checkRedisEndpoint(raw string) error {
	if len(raw) > 2048 {
		return errors.New("the Redis URL must be at most 2048 bytes")
	}
	return validateRedisEndpoint(raw)
}

func validateRedisEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "redis" && u.Scheme != "rediss") || u.Hostname() == "" || u.Opaque != "" || u.Fragment != "" || u.RawQuery != "" {
		return errors.New("enter a Redis URL such as redis://:password@redis:6379/0 (use rediss:// for TLS)")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("the Redis URL port must be between 1 and 65535")
		}
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); u.User.Username() != "" && !hasPassword {
			return errors.New("a Redis ACL username must be supplied with a password")
		}
	}
	if path := strings.TrimPrefix(u.Path, "/"); path != "" {
		if db, err := strconv.Atoi(path); err != nil || db < 0 {
			return errors.New("the Redis URL path must be a database number")
		}
	}
	return nil
}

// redisPlot is a server-rendered memory line chart. It uses the shared
// .chart-svg styles so it matches the other dashboard charts in both themes.
type redisPlot struct {
	Empty                bool
	Width, Height        float64
	PlotLeft, PlotRight  float64
	PlotTop, PlotBottom  float64
	TickX                float64
	Ticks                []redisTick
	Line, Area           string
	LastX, LastY         float64
	HasLimit             bool
	LimitY               float64
	LimitLabel           string
	LimitOffScale        bool
	StartLabel, EndLabel string
	Peak, Latest         int64
	MaxMemory            int64
	Label                string
}

type redisTick struct {
	Y     float64
	Label string
}

func makeRedisPlot(samples []store.RedisSample) redisPlot {
	p := redisPlot{Width: 720, Height: 210, PlotLeft: 70, PlotRight: 712, PlotTop: 12, PlotBottom: 182, TickX: 62}
	if len(samples) == 0 {
		p.Empty = true
		return p
	}
	for _, s := range samples {
		p.Peak = max(p.Peak, s.UsedMemory)
	}
	last := samples[len(samples)-1]
	p.Latest, p.MaxMemory = last.UsedMemory, last.MaxMemory

	// Scale to the observed peak with headroom. Include the memory limit when
	// it is close enough to be useful; otherwise a 10 MiB line against an
	// 8 GiB limit would flatten into the baseline.
	scale := max(p.Peak+p.Peak/10, 1)
	if p.MaxMemory > 0 {
		if p.MaxMemory <= scale*3/2 {
			scale = max(scale, p.MaxMemory)
			p.HasLimit = true
		} else {
			p.LimitOffScale = true
		}
		p.LimitLabel = humanBytes(p.MaxMemory)
	}
	plotH := p.PlotBottom - p.PlotTop
	y := func(v int64) float64 { return p.PlotBottom - float64(v)*plotH/float64(scale) }
	x := func(i int) float64 {
		if len(samples) == 1 {
			return p.PlotRight
		}
		return p.PlotLeft + float64(i)*(p.PlotRight-p.PlotLeft)/float64(len(samples)-1)
	}
	for _, f := range []int64{0, 1, 2} {
		v := scale * f / 2
		p.Ticks = append(p.Ticks, redisTick{Y: round1(y(v)), Label: humanBytes(v)})
	}
	if p.HasLimit {
		p.LimitY = round1(y(p.MaxMemory))
	}
	var line, area strings.Builder
	fmt.Fprintf(&area, "M%.1f,%.1f", x(0), p.PlotBottom)
	for i, s := range samples {
		if i > 0 {
			line.WriteByte(' ')
		}
		fmt.Fprintf(&line, "%.1f,%.1f", x(i), y(s.UsedMemory))
		fmt.Fprintf(&area, " L%.1f,%.1f", x(i), y(s.UsedMemory))
	}
	fmt.Fprintf(&area, " L%.1f,%.1f Z", x(len(samples)-1), p.PlotBottom)
	p.Line, p.Area = line.String(), area.String()
	p.LastX, p.LastY = round1(x(len(samples)-1)), round1(y(last.UsedMemory))
	p.StartLabel = samples[0].SampledAt.UTC().Format("Jan 2 15:04")
	p.EndLabel = last.SampledAt.UTC().Format("Jan 2 15:04")
	p.Label = fmt.Sprintf("Redis memory used from %s to %s UTC: peak %s, latest %s", p.StartLabel, p.EndLabel, humanBytes(p.Peak), humanBytes(p.Latest))
	return p
}
