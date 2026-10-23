package main

import (
	"testing"
	"time"
)

func TestPreviewClockIsExplicitAndValidated(t *testing.T) {
	f, err := fixtureAtArgument("2026-10-01T10:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	if !f.now.Equal(want) || !f.clock().Equal(want) {
		t.Fatal("fixture clock is not fixed")
	}
	if _, err = fixtureAtArgument("invalid"); err == nil {
		t.Fatal("invalid fixture time accepted")
	}
}
