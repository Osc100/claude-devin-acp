package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
// A longer one names the day too: "Your limit will reset in 6 hours 6 minutes (at Oct 31 06:01
// UTC)." That is not waited out within a turn: the turn ends saying so, and until it lifts every
// new turn is answered the same at once, without Devin.
//
// A limit is the model's that met it: SWE-2 is a free model, and "Reached free model rate limit"
// is the free models' limit, while GPT-6.1 Terra is paid usage, held by the paid limits below; so
// one model's limit leaves turns on the other to run.
//
// The paid limits name no time they lift, and are reported, not waited out: the daily quota
// ("Quota exhausted: Your daily usage quota has been exhausted. …", lifting the next day or with
// more usage bought), and, as the CLI words them, "You've reached your monthly usage limit. …",
// "Your organization has reached its monthly usage limit. …", "You've reached your ChatGPT
// subscription usage limit. …" and "An admin paused usage on your account. …" (the server's own
// words: "monthly acu limit reached", "organization usage limit reached", "usage frozen by an
// administrator").

// acpError is an error Devin answered a request with.
type acpError struct {
	method  string
	code    int
	message string
}

func (e *acpError) Error() string {
	return fmt.Sprintf("Devin refused %s: %s", e.method, e.message)
}

var resetsAt = regexp.MustCompile(`\(at (?:([A-Z][a-z]{2}) (\d{1,2}) )?(\d{1,2}):(\d{2}) UTC\)`)

// longestWait is the longest rate limit waited out within a turn.
const longestWait = 20 * time.Minute

// limit is a usage limit a turn ran into: a rate limit, lifting at until, or the quota.
type limit struct {
	// model is the Devin model whose turn met it, empty when not known.
	model   string
	rate    bool
	until   time.Time
	message string
}

// limitOf is the limit err, from a turn on model, reports, if it reports one.
func limitOf(err error, model string, now time.Time) (limit, bool) {
	var refused *acpError
	if !errors.As(err, &refused) {
		return limit{}, false
	}
	message := refused.message
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "rate limit"):
		l := limit{model: model, rate: true, until: now.Add(time.Minute), message: message}
		if found := resetsAt.FindStringSubmatch(message); found != nil {
			hour, _ := strconv.Atoi(found[3])
			minute, _ := strconv.Atoi(found[4])
			utc := now.UTC()
			at := time.Date(utc.Year(), utc.Month(), utc.Day(), hour, minute, 0, 0, time.UTC)
			if found[1] != "" {
				// A day named: that day this year, or next year's once this year's is past.
				day, _ := time.Parse("Jan 2 2006", fmt.Sprintf("%s %s %d", found[1], found[2], utc.Year()))
				at = time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, time.UTC)
				if at.Before(utc.AddDate(0, 0, -1)) {
					at = at.AddDate(1, 0, 0)
				}
			} else if at.Before(utc.Add(-time.Hour)) {
				at = at.AddDate(0, 0, 1) // past midnight UTC
			}
			l.until = at
		}
		return l, true
	case strings.Contains(lower, "quota exhausted"), strings.Contains(lower, "quota has been exhausted"),
		strings.Contains(lower, "usage limit"), strings.Contains(lower, "acu limit reached"),
		strings.Contains(lower, "paused usage"), strings.Contains(lower, "usage frozen"):
		return limit{model: model, message: message}, true
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

// long is whether a limit is one not waited out within a turn: the quota, or a rate limit lifting
// more than longestWait from now.
func (l limit) long(now time.Time) bool {
	return !l.rate || l.until.Sub(now) > longestWait
}

// Say is a limit as a turn's last line says it.
func (l limit) Say(now time.Time) string {
	if !l.rate {
		other := sweModel
		if l.model == sweModel {
			other = terraModel
		}
		return fmt.Sprintf("Devin's %s is out of usage, with no time it lifts; %s is not held by this. %s",
			modelName(l.model), modelName(other), l.message)
	}
	return fmt.Sprintf("Devin's %s is rate limited until %s (in %s), too long to wait within a turn; start this again then (`claude-devin-acp wait-limit --model %s` waits for it). %s",
		modelName(l.model), l.until.Local().Format("Jan 2 15:04"), l.until.Sub(now).Round(time.Minute), l.model, l.message)
}

// limitedUntil is a long limit Devin's model is under, if it is under one now: what a new turn on
// it is answered with, without Devin.
func (d *devin) limitedUntil(model string, now time.Time) (limit, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if l := d.longLimits[model]; l.rate && now.Before(l.until) {
		return l, true
	}
	return limit{}, false
}

// keptLimit is a long rate limit as it is kept on disk, by model, so a restart still knows it.
type keptLimit struct {
	Until   time.Time `json:"until"`
	Message string    `json:"message"`
}

func limitStore() string {
	cache, _ := cacheDir()
	return filepath.Join(cache, "limit.json")
}

// holdLimit has new turns on l's model answered with l until it lifts, and keeps it.
func (d *devin) holdLimit(l limit) {
	d.mu.Lock()
	d.longLimits[l.model] = l
	kept := map[string]keptLimit{}
	for model, l := range d.longLimits {
		kept[model] = keptLimit{l.until, l.message}
	}
	d.mu.Unlock()
	raw, _ := json.Marshal(kept)
	if err := os.WriteFile(limitStore(), raw, 0o600); err != nil {
		log.Printf("keeping the rate limit: %v", err)
	}
}

// loadLimit reads the long rate limits a previous run kept that have not lifted. One kept before
// limits were by model was SWE-2's, then the only model.
func (d *devin) loadLimit() {
	raw, err := os.ReadFile(limitStore())
	if err != nil {
		return
	}
	var kept map[string]keptLimit
	if json.Unmarshal(raw, &kept) != nil {
		var old keptLimit
		if json.Unmarshal(raw, &old) != nil {
			return
		}
		kept = map[string]keptLimit{sweModel: old}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for model, k := range kept {
		if time.Now().After(k.Until) {
			continue
		}
		d.longLimits[model] = limit{model: model, rate: true, until: k.Until, message: k.Message}
		log.Printf("Devin's %s is rate limited until %s, as kept from before", modelName(model), k.Until.Local().Format("Jan 2 15:04"))
	}
}

// limitWaits is how many rate limits in a row one turn waits out before giving up.
const limitWaits = 5

// continueAfterLimit is what Devin is told once a rate limit it stopped on has lifted.
const continueAfterLimit = "(You were stopped by a rate limit, which has now lifted. Continue where you left off.)"

// prompt runs a turn in a Devin session on model, and when Devin gives it up on a rate limit, waits until
// the limit lifts (telling waiting first) and has Devin go on. ctx ends the waiting.
func (d *devin) prompt(ctx context.Context, model, session, text string, waiting func(limit)) (map[string]any, error) {
	for attempt := 0; ; attempt++ {
		result, err := d.request("session/prompt", map[string]any{
			"sessionId": session, "prompt": []any{map[string]any{"type": "text", "text": text}},
		})
		now := time.Now()
		l, limited := limitOf(err, model, now)
		if limited && l.rate && l.long(now) {
			d.holdLimit(l)
			log.Printf("Devin's %s is rate limited until %s; not waiting that long", modelName(model), l.until.Local().Format("Jan 2 15:04"))
		}
		if !limited || l.long(now) || attempt == limitWaits {
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
