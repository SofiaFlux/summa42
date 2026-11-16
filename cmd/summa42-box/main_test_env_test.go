package main

import (
	"os"
	"testing"
)

// TestMain gives every test a dedicated model user that exists (it is never
// spawned: tests that start a model use climodel's own seams), so parsing the
// model flags passes the fail-closed run-as-user check.
//
// SUMMA42_HOME defaults to a temporary directory, so nothing a test runs
// (rejected model answers, profile lookups) reaches the Owner's ~/.summa42.
// Tests that need a Collective set their own with t.Setenv.
func TestMain(m *testing.M) {
	os.Setenv("SUMMA42_MODEL_USER", "nobody")
	home, err := os.MkdirTemp("", "summa42-box-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("SUMMA42_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
