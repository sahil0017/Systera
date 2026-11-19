package projects

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/systera/control-plane/internal/platform/ids"
)

var (
	ErrNotFound    = errors.New("project not found")
	ErrInvalidName = errors.New("project name is required")
)

const maxNameLen = 200

// Status represents the lifecycle state of a Project.
type Status string

const (
	StatusActive   Status = "ACTIVE"
	StatusArchived Status = "ARCHIVED"
)

// Project is a tenant-scoped container for architecture models, runs, and artifacts.
type Project struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedBy   string    `json:"created_by"`
	Status      Status    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// CorrelationID ties the creation to its request for audit; not persisted on the row.
	CorrelationID string `json:"-"`
}

// Repository defines the persistence port for projects.
type Repository interface {
	Create(ctx context.Context, p Project) (Project, error)
	GetByID(ctx context.Context, tenantID, projectID string) (Project, error)
	ListByTenant(ctx context.Context, tenantID string) ([]Project, error)
}

func validate(p *Project) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len(p.Name) > maxNameLen {
		return ErrInvalidName
	}
	return nil
}

// MemoryRepository is a thread-safe in-memory Repository for unit tests.
type MemoryRepository struct {
	mu       sync.RWMutex
	projects map[string]Project
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{projects: make(map[string]Project)}
}

func (r *MemoryRepository) Create(_ context.Context, p Project) (Project, error) {
	if err := validate(&p); err != nil {
		return Project{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	p.ID = ids.New()
	now := time.Now().UTC()
	p.Status = StatusActive
	p.CreatedAt = now
	p.UpdatedAt = now
	r.projects[p.ID] = p
	return p, nil
}

func (r *MemoryRepository) GetByID(_ context.Context, tenantID, projectID string) (Project, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.projects[projectID]
	if !ok || p.TenantID != tenantID {
		return Project{}, ErrNotFound
	}
	return p, nil
}

func (r *MemoryRepository) ListByTenant(_ context.Context, tenantID string) ([]Project, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Project, 0)
	for _, p := range r.projects {
		if p.TenantID == tenantID {
			out = append(out, p)
		}
	}
	return out, nil
}
