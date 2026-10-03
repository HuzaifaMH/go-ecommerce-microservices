package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"unicode"
)

// decodeJSON reads one JSON object from the request body into dst. Unknown
// fields are rejected, so a typo (or an attempt to set a field the API does
// not accept, such as customer_id) fails loudly instead of being ignored.
// When optional is true an empty body is accepted and leaves dst unchanged.
// It writes the error response itself and returns false on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, optional bool) bool {
	if r.Body == nil || r.ContentLength == 0 {
		if optional {
			return true
		}
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "a JSON request body is required")
		return false
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, r, http.StatusUnsupportedMediaType, codeUnsupportedMedia, "Content-Type must be application/json")
		return false
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			writeError(w, r, http.StatusRequestEntityTooLarge, codePayloadTooLarge, "the request body is too large")
		case errors.Is(err, io.EOF) && optional:
			return true
		default:
			writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "invalid JSON: "+err.Error())
		}
		return false
	}
	// Exactly one JSON value: anything after it is an error.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "the request body must contain a single JSON object")
		return false
	}
	return true
}

// queryPageSize reads ?page_size=. Absent means "let the service pick".
func queryPageSize(w http.ResponseWriter, r *http.Request) (int32, bool) {
	raw := r.URL.Query().Get("page_size")
	if raw == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || n < 0 {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "page_size must be a non-negative integer")
		return 0, false
	}
	return int32(n), true //nolint:gosec // parsed with a 32-bit size above
}

// printableASCII reports whether s is non-empty, at most max long, and made
// only of visible ASCII characters (no spaces or control characters).
func printableASCII(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for _, r := range s {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
