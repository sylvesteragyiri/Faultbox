package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const turnstileSiteverifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

type turnstileResult struct {
	Success  bool   `json:"success"`
	Hostname string `json:"hostname"`
	Action   string `json:"action"`
}

func (h *Handler) verifyTurnstile(ctx context.Context, token, hostname string) (bool, error) {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 2048 {
		return false, nil
	}

	form := url.Values{
		"secret":   {h.opts.TurnstileSecretKey},
		"response": {token},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, turnstileSiteverifyURL, strings.NewReader(form.Encode()))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := h.turnstileClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("siteverify returned HTTP %d", resp.StatusCode)
	}

	var result turnstileResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&result); err != nil {
		return false, err
	}

	return result.Success && result.Action == "login" && strings.EqualFold(result.Hostname, hostname), nil
}

func requestHostname(hostport string) string {
	if hostname, _, err := net.SplitHostPort(hostport); err == nil {
		hostport = hostname
	}
	return strings.TrimSuffix(strings.Trim(strings.ToLower(hostport), "[]"), ".")
}
