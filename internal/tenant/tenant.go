package tenant

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound      = errors.New("tenant not found")
	ErrAlreadyExists = errors.New("tenant already exists")
	slugRE           = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)
)

type Quota struct {
	MaxDeployments        int32  `json:"maxDeployments"`
	MaxGPUs               int32  `json:"maxGpus"`
	RequestsPerMinute     int32  `json:"requestsPerMinute"`
	MaxConcurrentRequests int32  `json:"maxConcurrentRequests"`
	MaxQueuedRequests     int32  `json:"maxQueuedRequests"`
	DefaultPriorityClass  string `json:"defaultPriorityClass"`
}

type Tenant struct {
	ID          string     `json:"id"`
	Slug        string     `json:"slug"`
	Name        string     `json:"name"`
	Namespace   string     `json:"namespace"`
	Quota       Quota      `json:"quota"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	SuspendedAt *time.Time `json:"suspendedAt,omitempty"`
}

type Repository interface {
	Create(context.Context, *Tenant) error
	Get(context.Context, string) (*Tenant, error)
	GetBySlug(context.Context, string) (*Tenant, error)
	List(context.Context, int, int) ([]Tenant, error)
	UpdateQuota(context.Context, string, Quota, time.Time) error
	SetSuspended(context.Context, string, *time.Time, time.Time) error
}

type Service struct {
	repository  Repository
	now         func() time.Time
	provisioner interface {
		Provision(context.Context, *Tenant) error
	}
}

type ServiceOption func(*Service)

func WithNamespaceProvisioner(provisioner interface {
	Provision(context.Context, *Tenant) error
}) ServiceOption {
	return func(service *Service) { service.provisioner = provisioner }
}

func NewService(repository Repository, options ...ServiceOption) *Service {
	service := &Service{repository: repository, now: time.Now}
	for _, option := range options {
		option(service)
	}
	return service
}

func (s *Service) Create(ctx context.Context, slug, name string, quota Quota) (*Tenant, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if err := validate(slug, name, quota); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	t := &Tenant{
		ID:        tenantID(),
		Slug:      slug,
		Name:      strings.TrimSpace(name),
		Namespace: "tenant-" + slug,
		Quota:     quota,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.repository.Create(ctx, t); err != nil {
		return nil, err
	}
	if s.provisioner != nil {
		if err := s.provisioner.Provision(ctx, t); err != nil {
			return t, err
		}
	}
	return t, nil
}

func (s *Service) Get(ctx context.Context, id string) (*Tenant, error) {
	return s.repository.Get(ctx, id)
}

func (s *Service) Suspend(ctx context.Context, id string) error {
	at := s.now().UTC()
	return s.repository.SetSuspended(ctx, id, &at, at)
}

func (s *Service) Resume(ctx context.Context, id string) error {
	return s.repository.SetSuspended(ctx, id, nil, s.now().UTC())
}

func (s *Service) ProvisionNamespace(ctx context.Context, id string) error {
	if s.provisioner == nil {
		return errors.New("tenant namespace provisioner is not configured")
	}
	value, err := s.repository.Get(ctx, id)
	if err != nil {
		return err
	}
	return s.provisioner.Provision(ctx, value)
}

func validate(slug, name string, quota Quota) error {
	if len(slug) == 0 || len(slug) > 56 || !slugRE.MatchString(slug) {
		return errors.New("tenant slug must be a DNS label of at most 56 characters")
	}
	if strings.TrimSpace(name) == "" {
		return errors.New("tenant name is required")
	}
	if quota.MaxDeployments < 1 || quota.MaxGPUs < 1 || quota.RequestsPerMinute < 1 || quota.MaxConcurrentRequests < 1 || quota.MaxQueuedRequests < 0 {
		return errors.New("tenant quota values are invalid")
	}
	switch quota.DefaultPriorityClass {
	case "interactive", "standard", "batch":
	default:
		return errors.New("default priority class must be interactive, standard, or batch")
	}
	return nil
}

func tenantID() string {
	id, err := uuid.NewV7()
	if err != nil {
		panic("generate tenant UUIDv7: " + err.Error())
	}
	return id.String()
}
