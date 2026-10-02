package valkey

import (
	"context"
	"crypto/tls"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func TestManagedValkeyConnection(t *testing.T) {
	options, err := connectionOptions(Config{
		URL:     "rediss://tenant:p%40ss%3Aword@cache.example.com:6380/2",
		Address: "unused:6379", Username: "unused", Password: "unused", Database: 9,
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.Addr != "cache.example.com:6380" || options.Username != "tenant" || options.Password != "p@ss:word" || options.DB != 2 {
		t.Fatal("managed URL did not override legacy address/auth/database or decode credentials")
	}
	if options.TLSConfig == nil || options.TLSConfig.InsecureSkipVerify || options.TLSConfig.MinVersion != tls.VersionTLS12 || options.TLSConfig.ServerName != "cache.example.com" {
		t.Fatal("managed rediss connection must verify the server certificate and require TLS 1.2 or newer")
	}
}

func TestValkeyConnectionRejectsUnsafeURLsWithoutLeakingCredentials(t *testing.T) {
	for _, value := range []string{
		"https://user:private-marker@cache.example.com",
		"rediss://user:private-marker@/0",
		"rediss://user:private-marker@cache.example.com/not-a-database",
		"rediss://user:private-marker@cache.example.com:bad/0",
		"rediss://user:private-marker@cache.example.com/0?skip_verify=true",
		"rediss://user:private-marker@cache.example.com/0#fragment",
		"rediss://user:private-marker@cache.example.com/0\n",
		"rediss://user:private-marker%XX@cache.example.com/0",
	} {
		_, err := connectionOptions(Config{URL: value})
		if err == nil || strings.Contains(err.Error(), "private-marker") {
			t.Fatalf("invalid connection must fail without leaking its password; error=%v", err)
		}
	}
}

func TestLegacyValkeyConnection(t *testing.T) {
	options, err := connectionOptions(Config{Address: "valkey:6379", Username: "local", Password: "secret", Database: 1})
	if err != nil || options.Addr != "valkey:6379" || options.Username != "local" || options.Password != "secret" || options.DB != 1 || options.TLSConfig != nil {
		t.Fatal("legacy local connection settings changed")
	}
	options, err = connectionOptions(Config{URL: "redis://valkey:6379/0"})
	if err != nil || options.TLSConfig != nil {
		t.Fatal("local redis URL should remain supported")
	}
}

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

func TestValkeyDurationInputsRejectSubMillisecondValues(t *testing.T) {
	client := &Client{}
	if _, err := client.AllowRate(context.Background(), "tenant", 1, time.Microsecond); err == nil {
		t.Fatal("sub-millisecond rate window was accepted")
	}
	if _, _, err := client.AcquireConcurrency(context.Background(), "tenant", 1, time.Microsecond); err == nil {
		t.Fatal("sub-millisecond concurrency TTL was accepted")
	}
	if _, err := client.PutIdempotency(context.Background(), "tenant", "request", nil, time.Microsecond); err == nil {
		t.Fatal("sub-millisecond idempotency TTL was accepted")
	}
	if _, _, err := client.AcquireLock(context.Background(), "namespace", "name", time.Microsecond); err == nil {
		t.Fatal("sub-millisecond lock TTL was accepted")
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
