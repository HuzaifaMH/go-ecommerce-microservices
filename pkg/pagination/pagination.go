// Package pagination implements opaque keyset page tokens for lists ordered
// newest first by (creation time, ID).
//
// A token carries the position of the last item of a page. Clients must treat
// it as opaque; the format may change.
package pagination

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Default and maximum page sizes applied by Size.
const (
	DefaultSize = 50
	MaxSize     = 200
)

// ErrInvalidToken is returned for a token that was not issued by Encode.
var ErrInvalidToken = errors.New("invalid page token")

// Size applies the default and the maximum to a requested page size. A
// negative size is rejected.
func Size(requested int32) (int, error) {
	switch {
	case requested < 0:
		return 0, errors.New("page size must not be negative")
	case requested == 0:
		return DefaultSize, nil
	case requested > MaxSize:
		return MaxSize, nil
	}
	return int(requested), nil
}

// Encode returns the token for the position (createdAt, id).
func Encode(createdAt time.Time, id string) string {
	raw := strconv.FormatInt(createdAt.UnixNano(), 10) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// Decode parses a token produced by Encode. An empty token means "first
// page" and returns ok == false.
func Decode(token string) (createdAt time.Time, id string, ok bool, err error) {
	if token == "" {
		return time.Time{}, "", false, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, "", false, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	nanos, id, found := strings.Cut(string(b), "|")
	if !found || id == "" {
		return time.Time{}, "", false, ErrInvalidToken
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return time.Time{}, "", false, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	return time.Unix(0, n).UTC(), id, true, nil
}
