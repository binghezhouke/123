package faults

import (
	"context"
	"errors"
	"testing"
)

func TestClassificationContract(t *testing.T) {
	if !IsTransient(context.DeadlineExceeded) || IsTransient(context.Canceled) {
		t.Fatal("deadline/cancel classification is incorrect")
	}
	if IsTransient(errors.New("unknown")) || KindOf(errors.New("unknown")) != InvalidResponse {
		t.Fatal("unknown errors should be permanent invalid responses")
	}
	transient := &Error{Kind: Throttled, Retryable: true}
	if !IsTransient(transient) || KindOf(transient) != Throttled {
		t.Fatal("typed transient classification was lost")
	}
}

func TestErrorRedactsURLsAndBearerTokens(t *testing.T) {
	err := (&Error{Kind: Network, Message: "request failed https://cdn.example/file?token=secret Bearer abc123"}).Error()
	if err == "" || contains(err, "cdn.example") || contains(err, "secret") || contains(err, "abc123") {
		t.Fatalf("Error() leaked sensitive text: %q", err)
	}
}

func contains(s, part string) bool {
	for i := 0; i+len(part) <= len(s); i++ {
		if s[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
