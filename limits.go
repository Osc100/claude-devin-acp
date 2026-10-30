package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Devin's usage limits, as its CLI 3000.11.234 reports them over ACP. A rate limit is retried by
// Devin itself, three attempts, and once those are spent the turn's session/prompt is answered with
// an error (code -32010) whose message says when the limit lifts:
//
//	Reached free model rate limit. Upgrade to Max for higher limits, or switch to a different
//	model. Your limit will reset in 2 minutes (at 06:44 UTC).
//
// The daily quota says "Quota exhausted: Your daily usage quota has been exhausted. …", and lifts
// only the next day or with more usage bought, so it is reported, not waited out.

// acpError is an error Devin answered a request with.
type acpError struct {
	method  string
	code    int
	message string
}

func (e *acpError) Error() string {
	return fmt.Sprintf("Devin refused %s: %s", e.method, e.message)
}

var resetsAt = regexp.MustCompile(`\(at (\d{1,2}):(\d{2}) UTC\)`)

// limit is a usage limit a turn ran into: a rate limit, lifting at until, or the quota.
type limit struct {
	rate    bool
	until   time.Time
	message string
}

// limitOf is the limit err reports, if it reports one.
func limitOf(err error, now time.Time) (limit, bool) {
	var refused *acpError
	if !errors.As(err, &refused) {
		return limit{}, false
	}
	message := refused.message
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "rate limit"):
		l := limit{rate: true, until: now.Add(time.Minute), message: message}
		if found := resetsAt.FindStringSubmatch(message); found != nil {
			hour, _ := strconv.Atoi(found[1])
			minute, _ := strconv.Atoi(found[2])
			utc := now.UTC()
			at := time.Date(utc.Year(), utc.Month(), utc.Day(), hour, minute, 0, 0, time.UTC)
			if at.Before(utc.Add(-time.Hour)) {
				at = at.AddDate(0, 0, 1) // past midnight UTC
			}
			l.until = at
		}
		return l, true
	case strings.Contains(lower, "quota exhausted"), strings.Contains(lower, "quota has been exhausted"):
		return limit{message: message}, true
	}
	return limit{}, false
}

// limitMargin is how much longer than Devin says a rate limit is waited out, so the next turn is
// not the one that finds it still in force; another margin for each limit met again right after
// one was waited out (1m, then 2m, 3m…).
const limitMargin = time.Minute

// resumeSpacing is how far apart sessions waiting out a rate limit go on: they are all told the
// same reset time, and going on at once would meet the limit again at once.
const resumeSpacing = 30 * time.Second

// resumeSlot is when a session waiting out a rate limit until at goes on: at, or resumeSpacing
// after the session that goes on last, whichever is later.
func (d *devin) resumeSlot(at time.Time) time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	if earliest := d.lastResume.Add(resumeSpacing); at.Before(earliest) {
		at = earliest
	}
	d.lastResume = at
	return at
}

// limitWaits is how many rate limits in a row one turn waits out before giving up.
const limitWaits = 5

// continueAfterLimit is what Devin is told once a rate limit it stopped on has lifted.
const continueAfterLimit = "(You were stopped by a rate limit, which has now lifted. Continue where you left off.)"

// prompt runs a turn in a Devin session, and when Devin gives it up on a rate limit, waits until
// the limit lifts (telling waiting first) and has Devin go on. ctx ends the waiting.
func (d *devin) prompt(ctx context.Context, session, text string, waiting func(limit)) (map[string]any, error) {
	for attempt := 0; ; attempt++ {
		result, err := d.request("session/prompt", map[string]any{
			"sessionId": session, "prompt": []any{map[string]any{"type": "text", "text": text}},
		})
		l, limited := limitOf(err, time.Now())
		if !limited || !l.rate || attempt == limitWaits {
			return result, err
		}
		l.until = d.resumeSlot(l.until.Add(limitMargin * time.Duration(attempt+1)))
		log.Printf("Devin session %s is rate limited; waiting until %s", session, l.until.Local().Format("15:04:05"))
		if waiting != nil {
			waiting(l)
		}
		select {
		case <-ctx.Done():
			return result, err
		case <-time.After(time.Until(l.until)):
		}
		text = continueAfterLimit
	}
}
