// Package ratelimit limits requests per client IP, with the counts in Redis
// so every pod of a service shares them.
package ratelimit

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// Each call has its own deadline, so a slow Redis costs a request at most
// this long before the middleware lets it through.
const callTimeout = 100 * time.Millisecond

// fixedWindow counts a request and returns the count and the window's
// remaining time in ms. The first request of a window starts its expiry; a
// key that somehow has none (ttl < 0) gets one instead of living forever.
// One script, so INCR and PEXPIRE can't be split by a crash.
var fixedWindow = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
  ttl = tonumber(ARGV[1])
end
return {n, ttl}
`)

// Result is one request's decision. Reset is how long until the window ends
// and the count starts over.
type Result struct {
	Allowed   bool
	Limit     int
	Remaining int
	Reset     time.Duration
}

// Limiter allows limit requests per key per fixed window. Keys are
// "ratelimit:<prefix>:<key>"; the prefix (the service name) gives each
// service its own budget.
type Limiter struct {
	rdb    *redis.Client
	prefix string
	limit  int
	window time.Duration
}

func New(rdb *redis.Client, prefix string, limit int, window time.Duration) *Limiter {
	return &Limiter{rdb: rdb, prefix: prefix, limit: limit, window: window}
}

// Allow counts one request for key and reports whether it's within the limit.
func (l *Limiter) Allow(ctx context.Context, key string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	vals, err := fixedWindow.Run(ctx, l.rdb, []string{"ratelimit:" + l.prefix + ":" + key}, l.window.Milliseconds()).Int64Slice()
	if err != nil {
		return Result{}, err
	}
	n, ttl := int(vals[0]), time.Duration(vals[1])*time.Millisecond
	return Result{
		Allowed:   n <= l.limit,
		Limit:     l.limit,
		Remaining: max(l.limit-n, 0),
		Reset:     ttl,
	}, nil
}
