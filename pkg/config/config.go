// Package config loads service configuration from environment variables.
//
// A Loader accumulates errors so a service reports every invalid or missing
// setting at startup in one message instead of failing on the first one.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Loader reads typed values from a lookup function (the environment by default).
type Loader struct {
	lookup func(string) (string, bool)
	errs   []error
}

// FromEnv returns a Loader backed by the process environment.
func FromEnv() *Loader {
	return &Loader{lookup: os.LookupEnv}
}

// FromMap returns a Loader backed by a map. It is intended for tests.
func FromMap(m map[string]string) *Loader {
	return &Loader{lookup: func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}}
}

// String returns the value of key, or def when it is unset or empty.
func (l *Loader) String(key, def string) string {
	if v, ok := l.lookup(key); ok && v != "" {
		return v
	}
	return def
}

// RequiredString returns the value of key and records an error when it is unset or empty.
func (l *Loader) RequiredString(key string) string {
	v, ok := l.lookup(key)
	if !ok || v == "" {
		l.errs = append(l.errs, fmt.Errorf("%s is required", key))
		return ""
	}
	return v
}

// Int returns key parsed as an int, or def when unset.
func (l *Loader) Int(key string, def int) int {
	v, ok := l.lookup(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s: invalid integer %q", key, v))
		return def
	}
	return n
}

// Bool returns key parsed as a bool, or def when unset.
func (l *Loader) Bool(key string, def bool) bool {
	v, ok := l.lookup(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s: invalid boolean %q", key, v))
		return def
	}
	return b
}

// Duration returns key parsed with time.ParseDuration (e.g. "5s"), or def when unset.
func (l *Loader) Duration(key string, def time.Duration) time.Duration {
	v, ok := l.lookup(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s: invalid duration %q", key, v))
		return def
	}
	return d
}

// Float returns key parsed as a float64, or def when unset.
func (l *Loader) Float(key string, def float64) float64 {
	v, ok := l.lookup(key)
	if !ok || v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s: invalid number %q", key, v))
		return def
	}
	return f
}

// Strings returns key split on commas, with surrounding spaces and empty
// items removed, or nil when unset.
func (l *Loader) Strings(key string) []string {
	v, ok := l.lookup(key)
	if !ok || v == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// Err returns all accumulated errors joined together, or nil.
func (l *Loader) Err() error {
	return errors.Join(l.errs...)
}
