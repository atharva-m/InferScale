package valkey

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

type Config struct {
	// URL takes precedence over the legacy address fields. rediss enables
	// certificate-verified TLS for externally managed Valkey services.
	URL      string
	Address  string
	Username string
	Password string
	Database int
	Prefix   string
}

type Client struct {
	redis  *redis.Client
	prefix string
}

func Open(ctx context.Context, config Config) (*Client, error) {
	if config.Prefix == "" {
		config.Prefix = "inferscale"
	}
	options, err := connectionOptions(config)
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(options)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping Valkey: %w", err)
	}
	return &Client{redis: client, prefix: config.Prefix}, nil
}

func connectionOptions(config Config) (*redis.Options, error) {
	if config.URL == "" {
		return &redis.Options{
			Addr: config.Address, Username: config.Username, Password: config.Password, DB: config.Database,
		}, nil
	}
	parsed, err := url.Parse(config.URL)
	if err != nil || parsed.Hostname() == "" ||
		(parsed.Scheme != "redis" && parsed.Scheme != "rediss") ||
		strings.ContainsAny(config.URL, "?#\r\n\t ") {
		// Parse errors can contain the URL, including its password.
		return nil, errors.New("invalid Valkey URL: use redis:// or rediss:// with a host and optional database, without query or fragment")
	}
	options, err := redis.ParseURL(config.URL)
	if err != nil {
		return nil, errors.New("invalid Valkey URL connection settings")
	}
	if parsed.Scheme == "rediss" {
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: parsed.Hostname()}
	}
	return options, nil
}

func New(client *redis.Client, prefix string) *Client {
	if prefix == "" {
		prefix = "inferscale"
	}
	return &Client{redis: client, prefix: prefix}
}

func (c *Client) Close() error { return c.redis.Close() }

func (c *Client) Ping(ctx context.Context) error { return c.redis.Ping(ctx).Err() }

type RateDecision struct {
	Allowed    bool
	Remaining  int64
	RetryAfter time.Duration
}

const rateScriptSource = `
local capacity = tonumber(ARGV[1])
local refill_window_ms = tonumber(ARGV[2])
local cost = 1

local now_parts = redis.call('TIME')
local now_ms = tonumber(now_parts[1]) * 1000 + math.floor(tonumber(now_parts[2]) / 1000)
local state = redis.call('HMGET', KEYS[1], 'tokens', 'updated_at_ms')
local tokens = capacity
local updated_at_ms = now_ms
if state[1] and state[2] then
  tokens = tonumber(state[1]) or capacity
  updated_at_ms = tonumber(state[2]) or now_ms
end

local elapsed_ms = now_ms - updated_at_ms
if elapsed_ms < 0 then elapsed_ms = 0 end
tokens = math.min(capacity, tokens + (elapsed_ms * capacity / refill_window_ms))

local allowed = 0
if tokens >= cost then
  allowed = 1
  tokens = tokens - cost
end

local retry_after_ms = 0
if allowed == 0 then
  retry_after_ms = math.ceil((cost - tokens) * refill_window_ms / capacity)
  if retry_after_ms < 1 then retry_after_ms = 1 end
end

redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'updated_at_ms', now_ms)
redis.call('PEXPIRE', KEYS[1], math.ceil(refill_window_ms * 2))
return {allowed, math.floor(tokens), retry_after_ms}
`

var rateScript = redis.NewScript(rateScriptSource)

func (c *Client) AllowRate(ctx context.Context, tenantID string, limit int64, window time.Duration) (RateDecision, error) {
	if tenantID == "" || limit < 1 || window < time.Millisecond {
		return RateDecision{}, errors.New("invalid rate limit")
	}
	result, err := rateScript.Run(ctx, c.redis, []string{c.key("rate", tenantID)}, limit, window.Milliseconds()).Slice()
	if err != nil {
		return RateDecision{}, err
	}
	if len(result) != 3 {
		return RateDecision{}, errors.New("unexpected rate limiter response")
	}
	allowed, allowedOK := result[0].(int64)
	remaining, remainingOK := result[1].(int64)
	retryAfterMS, retryOK := result[2].(int64)
	if !allowedOK || !remainingOK || !retryOK || (allowed != 0 && allowed != 1) || remaining < 0 {
		return RateDecision{}, errors.New("invalid token bucket response")
	}
	if allowed != 1 && retryAfterMS < 1 {
		return RateDecision{}, errors.New("invalid token bucket retry interval")
	}
	return RateDecision{Allowed: allowed == 1, Remaining: remaining, RetryAfter: time.Duration(retryAfterMS) * time.Millisecond}, nil
}

type Lease struct {
	Key   string
	Token string
}

var acquireConcurrencyScript = redis.NewScript(`
local now_parts = redis.call('TIME')
local now = tonumber(now_parts[1]) * 1000 + math.floor(tonumber(now_parts[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[1]) then return 0 end
redis.call('ZADD', KEYS[1], now + tonumber(ARGV[3]), ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return 1
`)

func (c *Client) AcquireConcurrency(ctx context.Context, tenantID string, limit int64, ttl time.Duration) (*Lease, bool, error) {
	if tenantID == "" || limit < 1 || ttl < time.Millisecond {
		return nil, false, errors.New("invalid concurrency limit")
	}
	token, err := uuid.NewV7()
	if err != nil {
		return nil, false, err
	}
	key := c.key("concurrency", tenantID)
	result, err := acquireConcurrencyScript.Run(ctx, c.redis, []string{key}, limit, token.String(), ttl.Milliseconds()).Int64()
	if err != nil {
		return nil, false, err
	}
	if result == 0 {
		return nil, false, nil
	}
	return &Lease{Key: key, Token: token.String()}, true, nil
}

func (c *Client) ReleaseConcurrency(ctx context.Context, lease *Lease) error {
	if lease == nil {
		return nil
	}
	return c.redis.ZRem(ctx, lease.Key, lease.Token).Err()
}

func (c *Client) PutIdempotency(ctx context.Context, tenantID, key string, value []byte, ttl time.Duration) (bool, error) {
	if tenantID == "" || key == "" || ttl < time.Millisecond {
		return false, errors.New("invalid idempotency key")
	}
	return c.redis.SetNX(ctx, c.key("idempotency", tenantID, key), value, ttl).Result()
}

func (c *Client) GetIdempotency(ctx context.Context, tenantID, key string) ([]byte, bool, error) {
	value, err := c.redis.Get(ctx, c.key("idempotency", tenantID, key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	return value, err == nil, err
}

var releaseLockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0
`)

func (c *Client) AcquireLock(ctx context.Context, namespace, name string, ttl time.Duration) (*Lease, bool, error) {
	if namespace == "" || name == "" || ttl < time.Millisecond {
		return nil, false, errors.New("invalid lock")
	}
	token, err := uuid.NewV7()
	if err != nil {
		return nil, false, err
	}
	key := c.key("lock", namespace, name)
	acquired, err := c.redis.SetNX(ctx, key, token.String(), ttl).Result()
	if err != nil || !acquired {
		return nil, acquired, err
	}
	return &Lease{Key: key, Token: token.String()}, true, nil
}

func (c *Client) ReleaseLock(ctx context.Context, lease *Lease) error {
	if lease == nil {
		return nil
	}
	_, err := releaseLockScript.Run(ctx, c.redis, []string{lease.Key}, lease.Token).Result()
	return err
}

func (c *Client) key(parts ...string) string {
	key := c.prefix
	for _, part := range parts {
		key += ":" + part
	}
	return key
}
