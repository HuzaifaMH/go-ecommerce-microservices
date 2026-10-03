package api

import (
	"net/http"
	"time"
)

const (
	defaultDevTokenTTL = time.Hour
	maxDevTokenTTL     = 24 * time.Hour
)

type devTokenRequest struct {
	Subject    string   `json:"subject"`
	Roles      []string `json:"roles"`
	TTLSeconds int      `json:"ttl_seconds"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// devToken mints a token for local development. It is only registered when
// the gateway runs with DEV_AUTH enabled, which must never be the case in
// production: anyone who can reach it can become any user, including an admin.
func (h *Handler) devToken(w http.ResponseWriter, r *http.Request) {
	var req devTokenRequest
	if !decodeJSON(w, r, &req, false) {
		return
	}
	if req.Subject == "" {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "subject is required")
		return
	}
	ttl := defaultDevTokenTTL
	if req.TTLSeconds != 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	if ttl <= 0 || ttl > maxDevTokenTTL {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "ttl_seconds must be between 1 and 86400")
		return
	}

	token, err := h.devIssuer.Issue(req.Subject, req.Roles, ttl)
	if err != nil {
		h.log.Error("issue dev token failed", "error", err)
		writeError(w, r, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{AccessToken: token, TokenType: "Bearer", ExpiresIn: int(ttl.Seconds())})
}
