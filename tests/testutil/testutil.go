// Package testutil holds helpers shared by the unit and feature test suites.
package testutil

import (
	"encoding/json"
	"io"
	"testing"
)

// DecodeJSON decodes r into a value of type T and fails the test on error.
func DecodeJSON[T any](t *testing.T, r io.Reader) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(r).Decode(&v); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return v
}
