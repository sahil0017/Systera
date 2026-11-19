package tenancy

import (
	"context"
	"errors"
)

var (
	// ErrMissingPrincipal is returned when a request lacks an authenticated tenant principal.
	ErrMissingPrincipal = errors.New("missing tenant principal in context")
	// ErrForbidden is returned when a principal attempts an unauthorized cross-tenant or role action.
	ErrForbidden = errors.New("forbidden")
)

// Role represents a tenant-scoped RBAC role.
type Role string

const (
	RoleOwner     Role = "OWNER"
	RoleAdmin     Role = "ADMIN"
	RoleArchitect Role = "ARCHITECT"
	RoleEngineer  Role = "ENGINEER"
	RoleViewer    Role = "VIEWER"
)

// CanWrite reports whether the role may create or modify tenant resources.
func (r Role) CanWrite() bool {
	switch r {
	case RoleOwner, RoleAdmin, RoleArchitect, RoleEngineer:
		return true
	default:
		return false
	}
}

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	return r.CanWrite() || r == RoleViewer
}

// Principal identifies the server-verified user, tenant and role for a request.
type Principal struct {
	TenantID string
	UserID   string
	Role     Role
	// Subject is the verified token subject; Scopes the token's scopes (empty = workspace token).
	Subject string
	Scopes  []string
}

type ctxKey string

const principalKey ctxKey = "systera_principal"

// WithPrincipal attaches a Principal to the context.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// PrincipalFromContext extracts the Principal from context.
func PrincipalFromContext(ctx context.Context) (Principal, error) {
	p, ok := ctx.Value(principalKey).(Principal)
	if !ok || p.TenantID == "" || p.UserID == "" {
		return Principal{}, ErrMissingPrincipal
	}
	return p, nil
}
