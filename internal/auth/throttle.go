package auth

import (
	"sync"
	"time"
)

const (
	freeAttempts = 3
	firstDelay   = time.Second
	longestDelay = 15 * time.Minute
	forgetAfter  = time.Hour
)

// Throttle slows down guessing: after three failed logins from one client,
// each further attempt must wait twice as long as the last, up to 15
// minutes. A success, or an hour without failures, starts over.
// ponytail: in memory and per process; a restart forgets the counts.
type Throttle struct {
	mutex    sync.Mutex
	failures map[string]record
}

type record struct {
	count int
	last  time.Time
}

func NewThrottle() *Throttle {
	return &Throttle{failures: map[string]record{}}
}

// Wait is how long client must wait before its next attempt.
func (throttle *Throttle) Wait(client string, now time.Time) time.Duration {
	throttle.mutex.Lock()
	defer throttle.mutex.Unlock()
	current, found := throttle.failures[client]
	stale := !found || now.Sub(current.last) > forgetAfter
	if stale || current.count < freeAttempts {
		return 0
	}
	delay := firstDelay << min(current.count-freeAttempts, 20)
	delay = min(delay, longestDelay)
	return max(current.last.Add(delay).Sub(now), 0)
}

func (throttle *Throttle) Fail(client string, now time.Time) {
	throttle.mutex.Lock()
	defer throttle.mutex.Unlock()
	current := throttle.failures[client]
	if now.Sub(current.last) > forgetAfter {
		current.count = 0
	}
	throttle.failures[client] = record{count: current.count + 1, last: now}
}

func (throttle *Throttle) Succeed(client string) {
	throttle.mutex.Lock()
	defer throttle.mutex.Unlock()
	delete(throttle.failures, client)
}
