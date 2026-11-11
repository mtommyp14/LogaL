package main

import (
	"testing"
	"time"
)

func TestCursorReconnectPreservesRepeatedOccurrences(t *testing.T) {
	var c logCursor
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.record(ts, "same")
	c.record(ts, "same")
	c.record(ts, "other")
	c.reconnect()
	for _, raw := range []string{"same", "other", "same"} {
		if !c.repeated(ts, raw) {
			t.Fatal("replayed line accepted")
		}
	}
	if c.repeated(ts, "same") {
		t.Fatal("new identical occurrence dropped")
	}
	c.record(ts, "same")
	next := ts.Add(time.Second)
	c.record(next, "new")
	if !c.repeated(ts, "old") {
		t.Fatal("old log accepted")
	}
	c.reconnect()
	if !c.repeated(next, "new") || c.repeated(next, "new") {
		t.Fatal("cursor did not advance")
	}
}
