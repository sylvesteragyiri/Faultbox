package dashboard

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/internal/store"
)

type redisPlot struct {
	Points   template.HTML
	Empty    bool
	MaxLabel string
	MidLabel string
}

func (h *Handler) redisPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	statuses, err := h.store.RedisStatuses(ctx)
	if err != nil {
		h.serverError(w, err)
		return
	}
	selectedID, _ := strconv.ParseInt(r.URL.Query().Get("target"), 10, 64)
	if selectedID == 0 && len(statuses) > 0 {
		selectedID = statuses[0].Target.ID
	}
	var selected *store.RedisStatus
	for i := range statuses {
		if statuses[i].Target.ID == selectedID {
			selected = &statuses[i]
			break
		}
	}
	var history []store.RedisSample
	var plot redisPlot
	if selected != nil {
		history, err = h.store.RedisHistory(ctx, selectedID, time.Now().AddDate(0, 0, -h.opts.RetentionDays))
		if err != nil {
			h.serverError(w, err)
			return
		}
		plot = makeRedisPlot(history)
	}
	pg := h.newPage(r, "Redis", "redis", map[string]any{
		"Statuses": statuses, "Selected": selected, "History": history, "Plot": plot,
		"RetentionDays": h.opts.RetentionDays,
	})
	pg.Flash = redisFlash(r.URL.Query().Get("done"))
	h.render(w, http.StatusOK, "redis", "", pg)
}

func redisFlash(code string) string {
	switch code {
	case "saved":
		return "Redis server saved."
	case "deleted":
		return "Redis server and its samples deleted."
	case "enabled":
		return "Redis monitoring enabled."
	case "disabled":
		return "Redis monitoring paused."
	}
	return ""
}

func (h *Handler) redisSave(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "The form could not be read.")
		return
	}
	name, err := store.NormalizeRedisName(r.PostFormValue("name"))
	if err != nil {
		h.errorPage(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	endpoint := strings.TrimSpace(r.PostFormValue("endpoint"))
	var sealed []byte
	if endpoint != "" {
		if len(endpoint) > 2048 {
			h.errorPage(w, r, http.StatusUnprocessableEntity, "Redis URL must be at most 2048 bytes.")
			return
		}
		if err := validateRedisEndpoint(endpoint); err != nil {
			h.errorPage(w, r, http.StatusUnprocessableEntity, err.Error())
			return
		}
		sealed, err = auth.Seal(h.opts.EncryptionKey, []byte(endpoint))
		if err != nil {
			h.serverError(w, err)
			return
		}
	} else if id == 0 {
		h.errorPage(w, r, http.StatusUnprocessableEntity, "Redis URL is required.")
		return
	}
	_, err = h.store.SaveRedisTarget(r.Context(), store.RedisTarget{
		ID: id, Name: name, Endpoint: sealed, Enabled: r.PostFormValue("enabled") == "1",
		AlertEnabled: r.PostFormValue("alert_enabled") == "1",
	})
	if err != nil {
		if err == store.ErrConflict {
			h.errorPage(w, r, http.StatusConflict, "A Redis server with that name already exists.")
			return
		}
		h.serverError(w, err)
		return
	}
	h.logger.Info("Redis monitoring target saved", "target_id", id, "user_id", h.userID(r))
	http.Redirect(w, r, "/redis?done=saved", http.StatusSeeOther)
}

func (h *Handler) redisDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		h.notFound(w, r)
		return
	}
	if err := h.store.DeleteRedisTarget(r.Context(), id); err != nil {
		h.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/redis?done=deleted", http.StatusSeeOther)
}

func validateRedisEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "redis" && u.Scheme != "rediss") || u.Hostname() == "" || u.Opaque != "" || u.Fragment != "" || u.RawQuery != "" {
		return fmt.Errorf("enter a Redis URL such as redis://:password@redis:6379/0 (use rediss:// for TLS)")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("Redis URL port must be between 1 and 65535")
		}
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); u.User.Username() != "" && !hasPassword {
			return fmt.Errorf("Redis ACL usernames must be supplied with a password")
		}
	}
	if path := strings.TrimPrefix(u.Path, "/"); path != "" {
		if db, err := strconv.Atoi(path); err != nil || db < 0 {
			return fmt.Errorf("Redis URL path must be a database number")
		}
	}
	return nil
}

func makeRedisPlot(samples []store.RedisSample) redisPlot {
	if len(samples) == 0 {
		return redisPlot{Empty: true}
	}
	const width, height = 720.0, 180.0
	var maxUsed int64
	for _, s := range samples {
		maxUsed = max(maxUsed, s.UsedMemory)
	}
	if maxUsed < 1 {
		maxUsed = 1
	}
	points := make([]string, 0, len(samples))
	for i, s := range samples {
		x := 8.0
		if len(samples) > 1 {
			x += float64(i) * (width - 16) / float64(len(samples)-1)
		}
		y := height - float64(s.UsedMemory)*(height-8)/float64(maxUsed)
		points = append(points, fmt.Sprintf("%.1f,%.1f", x, y))
	}
	// Values contain only locally generated numeric coordinates.
	return redisPlot{Points: template.HTML(strings.Join(points, " ")), MaxLabel: humanBytes(maxUsed), MidLabel: humanBytes(maxUsed / 2)}
}
