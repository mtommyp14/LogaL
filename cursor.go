package main

import "time"

// logCursor suppresses replay when kubectl reconnects with an inclusive timestamp.
// Counts preserve distinct occurrences of identical lines at the same timestamp.
type logCursor struct {
	last   time.Time
	counts map[string]int
	replay map[string]int
}

func (c *logCursor) reconnect() {
	c.replay = make(map[string]int, len(c.counts))
	for line, n := range c.counts {
		c.replay[line] = n
	}
}

func (c *logCursor) repeated(ts time.Time, raw string) bool {
	if ts.Before(c.last) {
		return true
	}
	if ts.Equal(c.last) && c.replay[raw] > 0 {
		c.replay[raw]--
		return true
	}
	return false
}

func (c *logCursor) record(ts time.Time, raw string) {
	if ts.After(c.last) {
		c.last = ts
		c.counts = make(map[string]int)
	}
	if c.counts == nil {
		c.counts = make(map[string]int)
	}
	c.counts[raw]++
}
