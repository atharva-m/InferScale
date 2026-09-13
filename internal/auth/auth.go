package auth

import (
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"
	"golang.org/x/sync/singleflight"
)

var (
	ErrUnauthenticated = errors.New("invalid or expired API key")
	ErrForbidden       = errors.New("API key does not grant the required scope")
	ErrKeyNotFound     = errors.New("API key not found")
	ErrUnavailable     = errors.New("authentication verification is busy")
)

const (
	ScopeDeploymentsRead  = "deployments:read"
	ScopeDeploymentsWrite = "deployments:write"
	ScopeBenchmarksRead   = "benchmarks:read"
	ScopeBenchmarksRun    = "benchmarks:run"
	ScopeInference        = "inference"
)

var defaultScopes = []string{
	ScopeDeploymentsRead,
	ScopeDeploymentsWrite,
	ScopeBenchmarksRead,
	ScopeBenchmarksRun,
	ScopeInference,
}

type Argon2Parameters struct {
	Iterations  uint32
	MemoryKiB   uint32
	Parallelism uint8
}

var defaultArgon2Parameters = Argon2Parameters{Iterations: 1, MemoryKiB: 64 * 1024, Parallelism: 4}

type Principal struct {
	APIKeyID          string
	TenantID          string
	TenantSlug        string
	TenantNamespace   string
	Scopes            []string
	TenantSuspendedAt *time.Time
}

func (p *Principal) HasScope(scope string) bool {
	for _, candidate := range p.Scopes {
		if candidate == scope {
			return true
		}
	}
	return false
}

type APIKey struct {
	ID         string
	TenantID   string
	Name       string
	Salt       []byte
	Hash       []byte
	Scopes     []string
	Argon2     Argon2Parameters
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
}

type Repository interface {
	CreateAPIKey(context.Context, *APIKey) error
	LookupAPIKey(context.Context, string) (*APIKey, *Principal, error)
	TouchAPIKey(context.Context, string, time.Time) error
	RevokeAPIKey(context.Context, string, string, time.Time) error
}

type Service struct {
	repository        Repository
	now               func() time.Time
	cache             *authenticationCache
	checks            singleflight.Group
	verificationSlots chan struct{}
	derive            func([]byte, []byte, Argon2Parameters) []byte
}

func NewService(repository Repository) *Service {
	return &Service{
		repository: repository,
		now:        time.Now,
		cache:      newAuthenticationCache(4096, 15*time.Second),
		// Two default verifiers consume 128 MiB, leaving room in the 512 MiB
		// service container for the cache, connections, and request handling.
		verificationSlots: make(chan struct{}, 2),
		derive:            derive,
	}
}

// CreateAPIKey returns the durable metadata and the raw key. The raw value is
// never persisted and must only be shown to an operator once.
func (s *Service) CreateAPIKey(ctx context.Context, tenantID, name string, scopes []string, expiresAt *time.Time) (*APIKey, string, error) {
	if tenantID == "" || strings.TrimSpace(name) == "" {
		return nil, "", errors.New("tenant and API key name are required")
	}
	now := s.now().UTC()
	if expiresAt != nil && !expiresAt.After(now) {
		return nil, "", errors.New("API key expiration must be in the future")
	}
	if len(scopes) == 0 {
		scopes = append([]string(nil), defaultScopes...)
	}
	if err := validateScopes(scopes); err != nil {
		return nil, "", err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, "", err
	}
	secret, err := randomBytes(32)
	if err != nil {
		return nil, "", err
	}
	salt, err := randomBytes(16)
	if err != nil {
		return nil, "", err
	}
	hash, err := s.deriveKey(ctx, secret, salt, defaultArgon2Parameters)
	if err != nil {
		return nil, "", err
	}
	key := &APIKey{
		ID: id.String(), TenantID: tenantID, Name: strings.TrimSpace(name),
		Salt: salt, Hash: hash, Scopes: append([]string(nil), scopes...),
		Argon2:    defaultArgon2Parameters,
		CreatedAt: now, ExpiresAt: expiresAt,
	}
	raw := encodeRawKey(key.ID, secret)
	if err := s.repository.CreateAPIKey(ctx, key); err != nil {
		return nil, "", err
	}
	return key, raw, nil
}

func (s *Service) Authenticate(ctx context.Context, raw string) (*Principal, error) {
	keyID, secret, ok := parseRawKey(raw)
	if !ok {
		return nil, ErrUnauthenticated
	}
	now := s.now().UTC()
	fingerprint := sha256.Sum256([]byte(raw))
	if principal, ok := s.cache.Get(fingerprint, now); ok {
		return principal, nil
	}
	result := s.checks.DoChan(string(fingerprint[:]), func() (any, error) {
		// A caller's cancellation must not cancel a verification shared by other
		// requests. Bound dependency work independently; each waiter can leave.
		checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return s.authenticateUncached(checkCtx, keyID, secret, fingerprint)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case checked := <-result:
		if checked.Err != nil {
			return nil, checked.Err
		}
		return clonePrincipal(checked.Val.(*Principal)), nil
	}
}

func (s *Service) authenticateUncached(ctx context.Context, keyID string, secret []byte, fingerprint [sha256.Size]byte) (*Principal, error) {
	now := s.now().UTC()
	if principal, ok := s.cache.Get(fingerprint, now); ok {
		return principal, nil
	}
	// Take the permit before the database lookup as well, so unique invalid
	// credentials cannot create an unbounded queue of dependency work.
	select {
	case s.verificationSlots <- struct{}{}:
		defer func() { <-s.verificationSlots }()
	default:
		return nil, ErrUnavailable
	}
	key, principal, err := s.repository.LookupAPIKey(ctx, keyID)
	if errors.Is(err, ErrKeyNotFound) || (err == nil && (key == nil || principal == nil)) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	if key.RevokedAt != nil || (key.ExpiresAt != nil && !key.ExpiresAt.After(now)) {
		return nil, ErrUnauthenticated
	}
	if principal.TenantSuspendedAt != nil {
		return nil, ErrUnauthenticated
	}
	actual := s.derive(secret, key.Salt, key.Argon2)
	if len(actual) != len(key.Hash) || subtle.ConstantTimeCompare(actual, key.Hash) != 1 {
		return nil, ErrUnauthenticated
	}
	principal.Scopes = append([]string(nil), key.Scopes...)
	cacheUntil := now.Add(s.cache.TTL())
	if key.ExpiresAt != nil && key.ExpiresAt.Before(cacheUntil) {
		cacheUntil = *key.ExpiresAt
	}
	s.cache.Put(fingerprint, principal, cacheUntil)
	_ = s.repository.TouchAPIKey(ctx, principal.APIKeyID, now)
	return principal, nil
}

func (s *Service) deriveKey(ctx context.Context, secret, salt []byte, parameters Argon2Parameters) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s.verificationSlots <- struct{}{}:
		defer func() { <-s.verificationSlots }()
		return s.derive(secret, salt, parameters), nil
	default:
		return nil, ErrUnavailable
	}
}

func (s *Service) Revoke(ctx context.Context, tenantID, keyID string) error {
	if err := s.repository.RevokeAPIKey(ctx, tenantID, keyID, s.now().UTC()); err != nil {
		return err
	}
	s.cache.DeleteKeyID(keyID)
	return nil
}

// authenticationCache stores only a SHA-256 fingerprint of a successfully
// authenticated raw credential and a defensive copy of its principal. It
// bounds both memory and revocation propagation: local revocation is evicted
// immediately, while suspension or revocation observed by another replica is
// rechecked after the short TTL.
type authenticationCache struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	entries  map[[sha256.Size]byte]*list.Element
	order    *list.List
}

type authenticationCacheEntry struct {
	fingerprint [sha256.Size]byte
	principal   Principal
	expiresAt   time.Time
}

func newAuthenticationCache(capacity int, ttl time.Duration) *authenticationCache {
	if capacity < 1 {
		capacity = 1
	}
	if ttl <= 0 {
		ttl = time.Second
	}
	return &authenticationCache{
		capacity: capacity, ttl: ttl,
		entries: make(map[[sha256.Size]byte]*list.Element, capacity), order: list.New(),
	}
}

func (c *authenticationCache) TTL() time.Duration {
	if c == nil {
		return 0
	}
	return c.ttl
}

func (c *authenticationCache) Get(fingerprint [sha256.Size]byte, now time.Time) (*Principal, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[fingerprint]
	if !ok {
		return nil, false
	}
	entry := element.Value.(*authenticationCacheEntry)
	if !entry.expiresAt.After(now) {
		c.remove(element)
		return nil, false
	}
	c.order.MoveToFront(element)
	return clonePrincipal(&entry.principal), true
}

func (c *authenticationCache) Put(fingerprint [sha256.Size]byte, principal *Principal, expiresAt time.Time) {
	if c == nil || principal == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.entries[fingerprint]; ok {
		entry := existing.Value.(*authenticationCacheEntry)
		entry.principal = *clonePrincipal(principal)
		entry.expiresAt = expiresAt
		c.order.MoveToFront(existing)
		return
	}
	entry := &authenticationCacheEntry{fingerprint: fingerprint, principal: *clonePrincipal(principal), expiresAt: expiresAt}
	element := c.order.PushFront(entry)
	c.entries[fingerprint] = element
	for len(c.entries) > c.capacity {
		c.remove(c.order.Back())
	}
}

func (c *authenticationCache) DeleteKeyID(keyID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for element := c.order.Back(); element != nil; {
		previous := element.Prev()
		if element.Value.(*authenticationCacheEntry).principal.APIKeyID == keyID {
			c.remove(element)
		}
		element = previous
	}
}

func (c *authenticationCache) remove(element *list.Element) {
	if element == nil {
		return
	}
	entry := element.Value.(*authenticationCacheEntry)
	delete(c.entries, entry.fingerprint)
	c.order.Remove(element)
}

func clonePrincipal(value *Principal) *Principal {
	if value == nil {
		return nil
	}
	copyValue := *value
	copyValue.Scopes = append([]string(nil), value.Scopes...)
	if value.TenantSuspendedAt != nil {
		suspendedAt := *value.TenantSuspendedAt
		copyValue.TenantSuspendedAt = &suspendedAt
	}
	return &copyValue
}

func RequireScope(principal *Principal, scope string) error {
	if principal == nil {
		return ErrUnauthenticated
	}
	if !principal.HasScope(scope) {
		return ErrForbidden
	}
	return nil
}

func encodeRawKey(id string, secret []byte) string {
	return "isk_" + id + "_" + base64.RawURLEncoding.EncodeToString(secret)
}

func parseRawKey(raw string) (string, []byte, bool) {
	parts := strings.SplitN(raw, "_", 3)
	if len(parts) != 3 || parts[0] != "isk" {
		return "", nil, false
	}
	if _, err := uuid.Parse(parts[1]); err != nil {
		return "", nil, false
	}
	secret, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(secret) != 32 {
		return "", nil, false
	}
	return parts[1], secret, true
}

func derive(secret, salt []byte, params Argon2Parameters) []byte {
	return argon2.IDKey(secret, salt, params.Iterations, params.MemoryKiB, params.Parallelism, 32)
}

func randomBytes(size int) ([]byte, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return nil, err
	}
	return value, nil
}

func validateScopes(scopes []string) error {
	for _, scope := range scopes {
		switch scope {
		case ScopeDeploymentsRead, ScopeDeploymentsWrite, ScopeBenchmarksRead, ScopeBenchmarksRun, ScopeInference:
		default:
			return errors.New("unsupported API key scope: " + scope)
		}
	}
	return nil
}
