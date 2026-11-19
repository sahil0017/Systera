package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/platform/postgres"
)

var ErrAgentNotFound = errors.New("agent definition not found or disabled")

// Definition is a registered specialist agent.
type Definition struct {
	AgentType    string
	DisplayName  string
	Model        string // empty: worker default
	SystemPrompt string
	AllowedTools []string
	MaxTokens    int
}

// Catalog resolves agent definitions.
type Catalog interface {
	Get(ctx context.Context, agentType string) (Definition, error)
}

// PostgresCatalog reads the agents table.
type PostgresCatalog struct{ pool *pgxpool.Pool }

func NewPostgresCatalog(pool *pgxpool.Pool) *PostgresCatalog { return &PostgresCatalog{pool: pool} }

func (c *PostgresCatalog) Get(ctx context.Context, agentType string) (Definition, error) {
	var d Definition
	err := postgres.WithSystemTx(ctx, c.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT agent_type, display_name, model, system_prompt, allowed_tools, max_tokens
			FROM agents WHERE agent_type = $1 AND status = 'ACTIVE'`, agentType).
			Scan(&d.AgentType, &d.DisplayName, &d.Model, &d.SystemPrompt, &d.AllowedTools, &d.MaxTokens)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Definition{}, fmt.Errorf("%w: %q", ErrAgentNotFound, agentType)
	}
	return d, err
}

// StaticCatalog is an in-memory Catalog (tests).
type StaticCatalog map[string]Definition

func (s StaticCatalog) Get(_ context.Context, agentType string) (Definition, error) {
	d, ok := s[agentType]
	if !ok {
		return Definition{}, fmt.Errorf("%w: %q", ErrAgentNotFound, agentType)
	}
	return d, nil
}
