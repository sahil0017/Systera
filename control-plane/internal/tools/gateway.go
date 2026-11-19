// Package tools is the capability-gated Tool Gateway. Agents request tool calls by name;
// the gateway executes a handler only after verifying, in order:
//  1. the tool is registered,
//  2. the tool is on the agent's allowlist,
//  3. the task was granted the tool's capability,
//  4. the per-task tool-call budget is not exhausted,
//  5. the arguments validate against the tool's input schema.
//
// Handler output is validated against the output schema when one is declared.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"sync"
	"time"
)

var (
	ErrUnknownTool        = errors.New("unknown tool")
	ErrToolNotAllowed     = errors.New("tool is not on the agent's allowlist")
	ErrCapabilityDenied   = errors.New("capability not granted to this task")
	ErrToolBudgetExceeded = errors.New("tool call budget exhausted")
	ErrInvalidArguments   = errors.New("invalid tool arguments")
	ErrInvalidOutput      = errors.New("tool returned output that violates its schema")
)

var toolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// Handler executes a tool. args have already been validated against the input schema.
type Handler func(ctx context.Context, inv Invocation, args map[string]any) (map[string]any, error)

// Tool is a registered capability-gated tool.
type Tool struct {
	Name         string
	Description  string
	Capability   string
	InputSchema  *Schema
	OutputSchema *Schema
	Timeout      time.Duration
	Handler      Handler
}

// Invocation identifies who is calling (for handlers and audit).
type Invocation struct {
	TenantID  string
	RunID     string
	TaskID    string
	AgentType string
	Tool      string
}

// Registry holds tool definitions.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

func (r *Registry) Register(t Tool) error {
	switch {
	case !toolNamePattern.MatchString(t.Name):
		return fmt.Errorf("tool name %q must be dotted lowercase (e.g. run.context)", t.Name)
	case t.Capability == "":
		return fmt.Errorf("tool %q: capability is required", t.Name)
	case t.InputSchema == nil || t.InputSchema.Type != "object":
		return fmt.Errorf("tool %q: input schema must be an object schema", t.Name)
	case t.Handler == nil:
		return fmt.Errorf("tool %q: handler is required", t.Name)
	}
	if t.Timeout <= 0 {
		t.Timeout = 30 * time.Second
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[t.Name]; dup {
		return fmt.Errorf("tool %q already registered", t.Name)
	}
	r.tools[t.Name] = t
	return nil
}

func (r *Registry) get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Policy is the effective permission set for one task attempt.
type Policy struct {
	AllowedTools        []string // from the agent definition
	GrantedCapabilities []string // from the task
	MaxCalls            int      // from the task budget
}

// Descriptor is what an agent is told about a tool it may call.
type Descriptor struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	InputSchema *Schema `json:"input_schema"`
}

// Gateway enforces policy over the registry.
type Gateway struct {
	reg    *Registry
	logger *slog.Logger
}

func NewGateway(reg *Registry, logger *slog.Logger) *Gateway {
	return &Gateway{reg: reg, logger: logger}
}

// Session tracks one attempt's tool usage under a fixed policy.
type Session struct {
	g       *Gateway
	inv     Invocation
	allowed map[string]bool
	granted map[string]bool
	max     int

	mu    sync.Mutex
	calls int
}

func (g *Gateway) NewSession(inv Invocation, p Policy) *Session {
	s := &Session{g: g, inv: inv, allowed: map[string]bool{}, granted: map[string]bool{}, max: p.MaxCalls}
	for _, t := range p.AllowedTools {
		s.allowed[t] = true
	}
	for _, c := range p.GrantedCapabilities {
		s.granted[c] = true
	}
	return s
}

// Available lists tools the session can actually call (allowlisted and granted).
func (s *Session) Available() []Descriptor {
	out := []Descriptor{}
	if s.max <= 0 {
		return out
	}
	for name := range s.allowed {
		if t, ok := s.g.reg.get(name); ok && s.granted[t.Capability] {
			out = append(out, Descriptor{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Calls returns how many tool calls were executed (successfully or not) in this session.
func (s *Session) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// Execute runs a tool call after every policy check passes. rawArgs is the JSON object
// the agent supplied.
func (s *Session) Execute(ctx context.Context, name string, rawArgs json.RawMessage) (map[string]any, error) {
	result, err := s.execute(ctx, name, rawArgs)
	level := slog.LevelInfo
	if err != nil {
		level = slog.LevelWarn
	}
	s.g.logger.Log(ctx, level, "tool call", "tool", name, "task_id", s.inv.TaskID, "agent_type", s.inv.AgentType, "error", err)
	return result, err
}

func (s *Session) execute(ctx context.Context, name string, rawArgs json.RawMessage) (map[string]any, error) {
	t, ok := s.g.reg.get(name)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTool, name)
	}
	if !s.allowed[name] {
		return nil, fmt.Errorf("%w: %q", ErrToolNotAllowed, name)
	}
	if !s.granted[t.Capability] {
		return nil, fmt.Errorf("%w: %q requires %q", ErrCapabilityDenied, name, t.Capability)
	}

	s.mu.Lock()
	if s.calls >= s.max {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w (%d allowed)", ErrToolBudgetExceeded, s.max)
	}
	s.calls++
	s.mu.Unlock()

	if len(bytes.TrimSpace(rawArgs)) == 0 {
		rawArgs = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(rawArgs))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
	}
	if err := t.InputSchema.Validate(decoded); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
	}
	args, _ := decoded.(map[string]any)

	inv := s.inv
	inv.Tool = name
	cctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	out, err := t.Handler(cctx, inv, args)
	if err != nil {
		return nil, fmt.Errorf("tool %q failed: %w", name, err)
	}
	if t.OutputSchema != nil {
		normalized, err := roundTrip(out)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidOutput, err)
		}
		if err := t.OutputSchema.Validate(normalized); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidOutput, err)
		}
	}
	return out, nil
}

func roundTrip(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	return out, dec.Decode(&out)
}
