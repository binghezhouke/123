// Package faults defines safe, stable classifications for failures returned by
// remote services. Callers should branch on Kind and Retryable, never on text.
package faults

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strings"
)

type Kind string

const (
	Network         Kind = "network"
	Throttled       Kind = "throttled"
	Unavailable     Kind = "unavailable"
	Unauthorized    Kind = "unauthorized"
	NotFound        Kind = "not_found"
	RemoteChanged   Kind = "remote_changed"
	InvalidResponse Kind = "invalid_response"
)

// Error carries a safe message and a machine-readable failure class.
type Error struct {
	Kind      Kind
	Message   string
	Retryable bool
}

var (
	urlRE    = regexp.MustCompile(`https?://[^\s"']+`)
	bearerRE = regexp.MustCompile(`(?i)bearer\s+[^\s,;]+`)
)

func (e *Error) Error() string {
	if e == nil {
		return "remote error"
	}
	message := strings.TrimSpace(e.Message)
	message = urlRE.ReplaceAllString(message, "[redacted URL]")
	message = bearerRE.ReplaceAllString(message, "Bearer [redacted]")
	if len(message) > 200 {
		message = message[:200]
	}
	if message == "" {
		return string(e.Kind)
	}
	return message
}

func IsTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var classified *Error
	if errors.As(err, &classified) {
		return classified.Retryable
	}
	var network net.Error
	return errors.As(err, &network)
}

func KindOf(err error) Kind {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return Network
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Network
	}
	var classified *Error
	if errors.As(err, &classified) {
		return classified.Kind
	}
	var network net.Error
	if errors.As(err, &network) {
		return Network
	}
	return InvalidResponse
}
