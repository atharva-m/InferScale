package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryRepository struct {
	mu        sync.Mutex
	key       *APIKey
	principal *Principal
	lookupErr error
	lookups   int
}

func (m *memoryRepository) CreateAPIKey(_ context.Context, key *APIKey) error {
	m.key = key
	return nil
}
func (m *memoryRepository) LookupAPIKey(_ context.Context, id string) (*APIKey, *Principal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lookups++
	if m.lookupErr != nil {
		return nil, nil, m.lookupErr
	}
	if m.key == nil || m.key.ID != id {
		return nil, nil, ErrKeyNotFound
	}
	copy := *m.principal
	return m.key, &copy, nil
}

func TestConcurrentAuthenticationCoalescesVerification(t *testing.T) {
	repository := &memoryRepository{principal: &Principal{TenantID: "tenant"}}
	service := NewService(repository)
	key, raw, err := service.CreateAPIKey(context.Background(), "tenant", "key", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	repository.principal.APIKeyID = key.ID
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	service.derive = func(secret, salt []byte, params Argon2Parameters) []byte {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return derive(secret, salt, params)
	}
	const requests = 16
	results := make(chan error, requests)
	for range requests {
		go func() { _, err := service.Authenticate(context.Background(), raw); results <- err }()
	}
	<-entered
	close(release)
	for range requests {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 || repository.lookups != 1 {
		t.Fatalf("verification calls=%d, database lookups=%d", calls.Load(), repository.lookups)
	}
}

func TestAuthenticationBoundsDistinctUncachedChecks(t *testing.T) {
	repository := &memoryRepository{principal: &Principal{TenantID: "tenant"}}
	service := NewService(repository)
	key, _, err := service.CreateAPIKey(context.Background(), "tenant", "key", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	repository.principal.APIKeyID = key.ID
	entered, release := make(chan struct{}, 2), make(chan struct{})
	service.derive = func(secret, salt []byte, params Argon2Parameters) []byte {
		entered <- struct{}{}
		<-release
		return derive(secret, salt, params)
	}
	results := make(chan error, 2)
	for i := byte(1); i <= 2; i++ {
		raw := encodeRawKey(key.ID, bytes.Repeat([]byte{i}, 32))
		go func() { _, err := service.Authenticate(context.Background(), raw); results <- err }()
	}
	<-entered
	<-entered
	_, err = service.Authenticate(context.Background(), encodeRawKey(key.ID, bytes.Repeat([]byte{3}, 32)))
	close(release)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("third check=%v, want controlled overload", err)
	}
	for range 2 {
		if err := <-results; !errors.Is(err, ErrUnauthenticated) {
			t.Fatal(err)
		}
	}
}

func TestAuthenticationWaiterCanCancelSharedCheck(t *testing.T) {
	repository := &memoryRepository{principal: &Principal{TenantID: "tenant"}}
	service := NewService(repository)
	key, raw, err := service.CreateAPIKey(context.Background(), "tenant", "key", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	repository.principal.APIKeyID = key.ID
	entered, release := make(chan struct{}), make(chan struct{})
	service.derive = func(secret, salt []byte, params Argon2Parameters) []byte {
		close(entered)
		<-release
		return derive(secret, salt, params)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := service.Authenticate(ctx, raw); result <- err }()
	<-entered
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter=%v", err)
	}
	close(release)
	if _, err := service.Authenticate(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
}
func (m *memoryRepository) TouchAPIKey(context.Context, string, time.Time) error { return nil }
func (m *memoryRepository) RevokeAPIKey(_ context.Context, _ string, keyID string, now time.Time) error {
	if m.key != nil && m.key.ID == keyID {
		m.key.RevokedAt = &now
	}
	return nil
}

func TestCreateAndAuthenticateAPIKey(t *testing.T) {
	repository := &memoryRepository{principal: &Principal{TenantID: "tenant", TenantNamespace: "tenant-acme"}}
	service := NewService(repository)
	key, raw, err := service.CreateAPIKey(context.Background(), "tenant", "ci", []string{ScopeDeploymentsRead}, nil)
	if err != nil {
		t.Fatal(err)
	}
	repository.principal.APIKeyID = key.ID
	if bytes.Contains([]byte(raw), key.Hash) || bytes.Contains([]byte(raw), key.Salt) {
		t.Fatal("raw API key exposes stored verifier")
	}
	principal, err := service.Authenticate(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !principal.HasScope(ScopeDeploymentsRead) {
		t.Fatal("scope missing")
	}
	if _, err := service.Authenticate(context.Background(), raw+"x"); err != ErrUnauthenticated {
		t.Fatalf("tampered key accepted: %v", err)
	}
}

func TestAPIKeyVerifierUsesRandomSalt(t *testing.T) {
	secret := bytes.Repeat([]byte{1}, 32)
	one, _ := randomBytes(16)
	two, _ := randomBytes(16)
	if bytes.Equal(derive(secret, one, defaultArgon2Parameters), derive(secret, two, defaultArgon2Parameters)) {
		t.Fatal("distinct salts produced identical verifiers")
	}
}

func TestAuthenticatePreservesRepositoryOutage(t *testing.T) {
	repository := &memoryRepository{principal: &Principal{TenantID: "tenant"}}
	service := NewService(repository)
	key, raw, err := service.CreateAPIKey(context.Background(), "tenant", "ci", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	repository.principal.APIKeyID = key.ID
	repositoryErr := errors.New("database unavailable")
	repository.lookupErr = repositoryErr
	if _, err := service.Authenticate(context.Background(), raw); !errors.Is(err, repositoryErr) {
		t.Fatalf("expected repository outage, got %v", err)
	}
}

func TestAuthenticateUsesBoundedPositiveCacheAndRechecksSuspension(t *testing.T) {
	repository := &memoryRepository{principal: &Principal{TenantID: "tenant", TenantNamespace: "tenant-acme"}}
	service := NewService(repository)
	now := time.Unix(1_000, 0).UTC()
	service.now = func() time.Time { return now }
	service.cache = newAuthenticationCache(2, 5*time.Second)
	key, raw, err := service.CreateAPIKey(context.Background(), "tenant", "ci", []string{ScopeInference}, nil)
	if err != nil {
		t.Fatal(err)
	}
	repository.principal.APIKeyID = key.ID
	first, err := service.Authenticate(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	first.Scopes[0] = ScopeDeploymentsWrite
	second, err := service.Authenticate(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if repository.lookups != 1 {
		t.Fatalf("repository lookups=%d, want one Argon/database verification", repository.lookups)
	}
	if !second.HasScope(ScopeInference) || second.HasScope(ScopeDeploymentsWrite) {
		t.Fatalf("cached principal was not defensively copied: %+v", second)
	}
	suspendedAt := now
	repository.principal.TenantSuspendedAt = &suspendedAt
	now = now.Add(6 * time.Second)
	if _, err := service.Authenticate(context.Background(), raw); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired cache did not recheck tenant suspension: %v", err)
	}
	if repository.lookups != 2 {
		t.Fatalf("repository lookups=%d after cache expiry, want two", repository.lookups)
	}
}

func TestRevokeImmediatelyEvictsLocalAuthenticationCache(t *testing.T) {
	repository := &memoryRepository{principal: &Principal{TenantID: "tenant"}}
	service := NewService(repository)
	key, raw, err := service.CreateAPIKey(context.Background(), "tenant", "ci", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	repository.principal.APIKeyID = key.ID
	if _, err := service.Authenticate(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	if err := service.Revoke(context.Background(), "tenant", key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(context.Background(), raw); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked cached key accepted: %v", err)
	}
	if repository.lookups != 2 {
		t.Fatalf("repository lookups=%d, want cache eviction and recheck", repository.lookups)
	}
}

func TestAuthenticationCacheEvictsLeastRecentlyUsedEntry(t *testing.T) {
	cache := newAuthenticationCache(2, time.Minute)
	now := time.Unix(2_000, 0).UTC()
	one := sha256.Sum256([]byte("one"))
	two := sha256.Sum256([]byte("two"))
	three := sha256.Sum256([]byte("three"))
	cache.Put(one, &Principal{APIKeyID: "one"}, now.Add(time.Minute))
	cache.Put(two, &Principal{APIKeyID: "two"}, now.Add(time.Minute))
	if _, ok := cache.Get(one, now); !ok {
		t.Fatal("first entry unexpectedly missing")
	}
	cache.Put(three, &Principal{APIKeyID: "three"}, now.Add(time.Minute))
	if _, ok := cache.Get(two, now); ok {
		t.Fatal("least recently used entry was retained past capacity")
	}
	if _, ok := cache.Get(one, now); !ok {
		t.Fatal("recently used entry was evicted")
	}
}
