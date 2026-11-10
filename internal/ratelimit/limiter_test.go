package ratelimit

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// newTestLimiter returns a limiter on TEST_REDIS_URL, skipping the test when
// it isn't set (make test sets it, see the Makefile). Keys get a prefix
// unique to the test, and are deleted afterwards.
func newTestLimiter(t *testing.T, limit int, window time.Duration) *Limiter {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opts)
	prefix := "test-" + strings.ReplaceAll(t.Name(), "/", "-") + "-" + time.Now().Format("150405.000000")
	t.Cleanup(func() {
		ctx := context.Background()
		keys, _ := rdb.Keys(ctx, "ratelimit:"+prefix+":*").Result()
		if len(keys) > 0 {
			rdb.Del(ctx, keys...)
		}
		_ = rdb.Close()
	})
	return New(rdb, prefix, limit, window)
}

func TestLimiterAllowsUpToTheLimit(t *testing.T) {
	l := newTestLimiter(t, 3, time.Minute)
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		res, err := l.Allow(ctx, "10.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		if !res.Allowed || res.Remaining != 3-i || res.Limit != 3 {
			t.Fatalf("request %d: got %+v, want allowed with %d remaining", i, res, 3-i)
		}
		if res.Reset <= 0 || res.Reset > time.Minute {
			t.Fatalf("request %d: reset %s, want within the 1m window", i, res.Reset)
		}
	}
	res, err := l.Allow(ctx, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed || res.Remaining != 0 {
		t.Fatalf("request 4: got %+v, want limited", res)
	}

	// Another IP has its own count.
	if res, _ := l.Allow(ctx, "10.0.0.2"); !res.Allowed || res.Remaining != 2 {
		t.Fatalf("another IP: got %+v, want allowed with 2 remaining", res)
	}
}

func TestLimiterWindowResets(t *testing.T) {
	l := newTestLimiter(t, 1, 200*time.Millisecond)
	ctx := context.Background()

	if res, err := l.Allow(ctx, "10.0.0.1"); err != nil || !res.Allowed {
		t.Fatalf("first: got %+v, %v, want allowed", res, err)
	}
	if res, _ := l.Allow(ctx, "10.0.0.1"); res.Allowed {
		t.Fatalf("second: got %+v, want limited", res)
	}
	time.Sleep(300 * time.Millisecond)
	if res, err := l.Allow(ctx, "10.0.0.1"); err != nil || !res.Allowed {
		t.Fatalf("after the window: got %+v, %v, want allowed", res, err)
	}
}

func TestLimiterRedisDown(t *testing.T) {
	// Nothing listens on port 1: the call must fail, not hang.
	l := New(redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1}), "test", 1, time.Minute)
	start := time.Now()
	if _, err := l.Allow(context.Background(), "10.0.0.1"); err == nil {
		t.Fatal("got no error with Redis down")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %s, want it bounded by the call timeout", elapsed)
	}
}
