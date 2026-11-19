package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/tenancy"
)

var (
	ErrUnauthenticated = errors.New("authentication required")
	// ErrNoMembership means the identity is valid but not an active member of the tenant.
	ErrNoMembership = errors.New("no active membership for tenant")
	// ErrTenantAmbiguous means the user belongs to several tenants and none was selected.
	ErrTenantAmbiguous = errors.New("user belongs to multiple tenants; select one with the tid claim or X-Tenant-ID")
)

// ScopeLens is the Engineering Lens extension scope. Lens tokens must be short-lived and
// pinned to one tenant, and may only reach the browser-intelligence routes.
const ScopeLens = "lens"

// MaxScopedTokenLifetime bounds exp - iat for any scoped token.
const MaxScopedTokenLifetime = time.Hour

// ScopeAllows reports whether a token with the given scopes may call path. Unscoped
// (workspace) tokens may call anything; scoped tokens only their scope's routes.
func ScopeAllows(scopes []string, path string) bool {
	if len(scopes) == 0 {
		return true
	}
	for _, sc := range scopes {
		if sc == ScopeLens && (strings.HasPrefix(path, "/v1/browser/") || path == "/v1/websites" || strings.HasPrefix(path, "/v1/websites/")) {
			return true
		}
	}
	return false
}

// Membership is a verified (user, tenant, role) binding loaded from the system of record.
type Membership struct {
	UserID   string
	TenantID string
	Role     tenancy.Role
}

// MembershipResolver maps a verified token subject to its tenant membership. When
// tenantID is empty the user's single active membership is used.
type MembershipResolver interface {
	Resolve(ctx context.Context, subject, tenantID string) (Membership, error)
}

// Authenticator turns an Authorization header into a verified Principal.
type Authenticator struct {
	verifier *Verifier
	members  MembershipResolver
}

func NewAuthenticator(v *Verifier, m MembershipResolver) *Authenticator {
	return &Authenticator{verifier: v, members: m}
}

// Authenticate verifies the bearer token and the membership. requestedTenant (from the
// X-Tenant-ID header) only *selects* among the caller's verified memberships; it cannot
// grant access, and it must agree with a tenant pinned in the token.
func (a *Authenticator) Authenticate(ctx context.Context, authorization, requestedTenant string) (tenancy.Principal, error) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(authorization), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return tenancy.Principal{}, ErrUnauthenticated
	}
	claims, err := a.verifier.VerifyContext(ctx, strings.TrimSpace(token))
	if err != nil {
		return tenancy.Principal{}, ErrUnauthenticated
	}

	if scopes := claims.Scopes(); len(scopes) > 0 {
		// Scoped tokens are short-lived by construction and pinned to a tenant.
		if claims.IssuedAt == 0 || claims.TenantID == "" ||
			time.Unix(claims.ExpiresAt, 0).Sub(time.Unix(claims.IssuedAt, 0)) > MaxScopedTokenLifetime {
			return tenancy.Principal{}, ErrUnauthenticated
		}
		for _, sc := range scopes {
			if sc != ScopeLens {
				return tenancy.Principal{}, ErrUnauthenticated // unknown scopes are refused, not ignored
			}
		}
	}

	tenantID := claims.TenantID
	if requestedTenant != "" {
		if tenantID != "" && !strings.EqualFold(tenantID, requestedTenant) {
			return tenancy.Principal{}, ErrNoMembership
		}
		tenantID = requestedTenant
	}
	if tenantID != "" && !ids.Valid(tenantID) {
		return tenancy.Principal{}, ErrNoMembership
	}

	m, err := a.members.Resolve(ctx, claims.Subject, strings.ToLower(tenantID))
	if err != nil {
		return tenancy.Principal{}, err
	}
	return tenancy.Principal{TenantID: m.TenantID, UserID: m.UserID, Role: m.Role, Subject: claims.Subject, Scopes: claims.Scopes()}, nil
}

// PostgresMemberships resolves memberships from users / tenant_memberships / tenants.
type PostgresMemberships struct {
	pool *pgxpool.Pool
}

func NewPostgresMemberships(pool *pgxpool.Pool) *PostgresMemberships {
	return &PostgresMemberships{pool: pool}
}

func (p *PostgresMemberships) Resolve(ctx context.Context, subject, tenantID string) (Membership, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT u.id::text, m.tenant_id::text, m.role
		FROM users u
		JOIN tenant_memberships m ON m.user_id = u.id
		JOIN tenants t ON t.id = m.tenant_id
		WHERE u.external_sub = $1
		  AND u.status = 'ACTIVE'
		  AND t.status = 'ACTIVE'
		  AND ($2 = '' OR m.tenant_id::text = $2)
		ORDER BY m.tenant_id
		LIMIT 2`, subject, tenantID)
	if err != nil {
		return Membership{}, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Membership, error) {
		var m Membership
		err := row.Scan(&m.UserID, &m.TenantID, &m.Role)
		return m, err
	})
	if err != nil {
		return Membership{}, err
	}
	return pick(list, tenantID)
}

// MemoryMemberships is an in-memory MembershipResolver for tests.
type MemoryMemberships struct {
	mu   sync.RWMutex
	byID map[string][]Membership // subject -> memberships
}

func NewMemoryMemberships() *MemoryMemberships {
	return &MemoryMemberships{byID: make(map[string][]Membership)}
}

func (m *MemoryMemberships) Add(subject string, mem Membership) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byID[subject] = append(m.byID[subject], mem)
}

func (m *MemoryMemberships) Resolve(_ context.Context, subject, tenantID string) (Membership, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var list []Membership
	for _, mem := range m.byID[subject] {
		if tenantID == "" || mem.TenantID == tenantID {
			list = append(list, mem)
		}
	}
	return pick(list, tenantID)
}

func pick(list []Membership, tenantID string) (Membership, error) {
	switch {
	case len(list) == 0:
		return Membership{}, ErrNoMembership
	case len(list) > 1 && tenantID == "":
		return Membership{}, ErrTenantAmbiguous
	default:
		return list[0], nil
	}
}
