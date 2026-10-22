package main

import (
	"os"
	"testing"
)

// TestMain gives every test a dedicated model user that exists (it is never
// spawned: tests that start a model use climodel's own seams), so parsing the
// model flags passes the fail-closed run-as-user check.
func TestMain(m *testing.M) {
	os.Setenv("SUMMA42_MODEL_USER", "nobody")
	os.Exit(m.Run())
}
