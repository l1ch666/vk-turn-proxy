package main

import (
	"strings"
	"testing"
)

func TestReadResponseBodyLimited(t *testing.T) {
	t.Run("accepts body at limit", func(t *testing.T) {
		body, err := readResponseBodyLimited(strings.NewReader("1234"), 4, "test response")
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if got := string(body); got != "1234" {
			t.Fatalf("body = %q, want %q", got, "1234")
		}
	})

	t.Run("rejects body over limit", func(t *testing.T) {
		_, err := readResponseBodyLimited(strings.NewReader("12345"), 4, "test response")
		if err == nil || !strings.Contains(err.Error(), "test response exceeds 4 bytes") {
			t.Fatalf("error = %v, want size-limit error", err)
		}
	})
}
