package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/gateway/internal/requestid"
)

// Error codes returned in the JSON error body. They are part of the REST contract.
const (
	codeInvalidArgument    = "invalid_argument"
	codeUnauthenticated    = "unauthenticated"
	codeNotFound           = "not_found"
	codeAlreadyExists      = "already_exists"
	codeFailedPrecondition = "failed_precondition"
	codeRateLimited        = "rate_limited"
	codeUnavailable        = "unavailable"
	codeTimeout            = "timeout"
	codeInternal           = "internal"
	codePayloadTooLarge    = "payload_too_large"
	codeUnsupportedMedia   = "unsupported_media_type"
)

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message, RequestID: requestid.From(r.Context())}})
}

// writeGRPCError turns an error from a backend service into the matching HTTP
// response. Backend messages are passed on for the codes that describe the
// caller's mistake; for everything else the details are logged, not leaked.
func (h *Handler) writeGRPCError(w http.ResponseWriter, r *http.Request, op string, err error) {
	// The client gave up (or our own deadline passed); gRPC reports it as a status
	// error, but a bare context error can also surface from interceptors.
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		writeError(w, r, http.StatusRequestTimeout, codeTimeout, "request canceled")
		return
	}

	st, ok := status.FromError(err)
	if !ok {
		h.log.Error("backend call failed", "op", op, "request_id", requestid.From(r.Context()), "error", err)
		writeError(w, r, http.StatusInternalServerError, codeInternal, "internal error")
		return
	}

	switch st.Code() {
	case codes.InvalidArgument:
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, st.Message())
	case codes.NotFound:
		writeError(w, r, http.StatusNotFound, codeNotFound, st.Message())
	case codes.AlreadyExists:
		writeError(w, r, http.StatusConflict, codeAlreadyExists, st.Message())
	case codes.FailedPrecondition:
		writeError(w, r, http.StatusConflict, codeFailedPrecondition, st.Message())
	case codes.Unavailable:
		h.log.Warn("backend unavailable", "op", op, "request_id", requestid.From(r.Context()), "error", err)
		writeError(w, r, http.StatusServiceUnavailable, codeUnavailable, "the service is temporarily unavailable, please retry")
	case codes.DeadlineExceeded:
		h.log.Warn("backend timed out", "op", op, "request_id", requestid.From(r.Context()))
		writeError(w, r, http.StatusGatewayTimeout, codeTimeout, "the request took too long, please retry")
	case codes.Canceled:
		writeError(w, r, http.StatusRequestTimeout, codeTimeout, "request canceled")
	default:
		h.log.Error("backend call failed", "op", op, "request_id", requestid.From(r.Context()), "code", st.Code().String(), "error", err)
		writeError(w, r, http.StatusInternalServerError, codeInternal, "internal error")
	}
}
