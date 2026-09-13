package valkey

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func TestKeyNamespacing(t *testing.T) {
	client := &Client{prefix: "test"}
	if got := client.key("rate", "tenant"); got != "test:rate:tenant" {
		t.Fatalf("unexpected key: %s", got)
	}
}

func TestRateLimiterIsAnAtomicTokenBucketScript(t *testing.T) {
	t.Parallel()
	for _, primitive := range []string{"redis.call('TIME')", "HMGET", "HSET", "elapsed_ms", "retry_after_ms"} {
		if !strings.Contains(rateScriptSource, primitive) {
			t.Fatalf("token bucket script is missing %q", primitive)
		}
	}
	if strings.Contains(rateScriptSource, "INCR") {
		t.Fatal("rate limiter regressed to a fixed-window counter")
	}
}

func TestAllowRateTokenBucketAgainstValkey(t *testing.T) {
	address := os.Getenv("INFERSCALE_TEST_VALKEY_ADDR")
	if address == "" {
		t.Skip("set INFERSCALE_TEST_VALKEY_ADDR to exercise the atomic Lua token bucket")
	}
	ctx := context.Background()
	raw := redis.NewClient(&redis.Options{Addr: address})
	t.Cleanup(func() { _ = raw.Close() })
	if err := raw.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping test Valkey: %v", err)
	}
	prefix := "inferscale-test-" + uuid.NewString()
	client := New(raw, prefix)
	t.Cleanup(func() { _ = raw.Del(ctx, client.key("rate", "concurrent"), client.key("rate", "refill")).Err() })

	var allowed atomic.Int64
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			decision, err := client.AllowRate(ctx, "concurrent", 5, time.Minute)
			if err != nil {
				t.Errorf("AllowRate: %v", err)
				return
			}
			if decision.Allowed {
				allowed.Add(1)
			}
		}()
	}
	group.Wait()
	if got := allowed.Load(); got != 5 {
		t.Fatalf("atomic admissions = %d, want capacity 5", got)
	}

	key := client.key("rate", "refill")
	now, err := raw.Time(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.HSet(ctx, key, "tokens", "0", "updated_at_ms", now.Add(-30*time.Second).UnixMilli()).Err(); err != nil {
		t.Fatal(err)
	}
	decision, err := client.AllowRate(ctx, "refill", 2, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Allowed || decision.Remaining != 0 || decision.RetryAfter != 0 {
		t.Fatalf("refilled decision = %#v, want exactly one available token", decision)
	}
	decision, err = client.AllowRate(ctx, "refill", 2, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Allowed || decision.RetryAfter <= 0 || decision.RetryAfter > 31*time.Second {
		t.Fatalf("denied decision = %#v, want a bounded retry interval", decision)
	}
}
