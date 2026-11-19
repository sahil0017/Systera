package agent

import (
	"context"
	"errors"

	"github.com/systera/control-plane/internal/runtime"
	"github.com/systera/control-plane/internal/tools"
)

type leaseKey struct{}

func withLease(ctx context.Context, l *runtime.Lease) context.Context {
	return context.WithValue(ctx, leaseKey{}, l)
}

func leaseFrom(ctx context.Context) (*runtime.Lease, error) {
	l, ok := ctx.Value(leaseKey{}).(*runtime.Lease)
	if !ok {
		return nil, errors.New("tool invoked outside a task attempt")
	}
	return l, nil
}

// Capabilities used by the built-in tools.
const (
	CapRunRead  = "run.read"
	CapTaskRead = "task.read"
)

// RegisterBuiltinTools adds the read-only context tools every agent may be allowlisted for.
// They only expose data already scoped to the caller's lease (tenant, run, upstream tasks).
func RegisterBuiltinTools(reg *tools.Registry) error {
	if err := reg.Register(tools.Tool{
		Name:        "run.context",
		Description: "Returns the run's overall objective, constraints and acceptance criteria.",
		Capability:  CapRunRead,
		InputSchema: tools.MustSchema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, _ tools.Invocation, _ map[string]any) (map[string]any, error) {
			l, err := leaseFrom(ctx)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"run_id":              l.Run.RunID,
				"objective":           l.Run.Objective,
				"constraints":         l.Run.Constraints,
				"acceptance_criteria": l.Run.AcceptanceCriteria,
			}, nil
		},
	}); err != nil {
		return err
	}
	return reg.Register(tools.Tool{
		Name:        "task.dependency_results",
		Description: "Returns the results of upstream tasks this task depends on, optionally filtered by task key.",
		Capability:  CapTaskRead,
		InputSchema: tools.MustSchema(`{"type":"object","properties":{"task_key":{"type":"string","minLength":1,"maxLength":63}}}`),
		Handler: func(ctx context.Context, _ tools.Invocation, args map[string]any) (map[string]any, error) {
			l, err := leaseFrom(ctx)
			if err != nil {
				return nil, err
			}
			key, _ := args["task_key"].(string)
			out := []runtime.DependencyResult{}
			for _, d := range l.Dependencies {
				if key == "" || d.Key == key {
					out = append(out, d)
				}
			}
			return map[string]any{"dependencies": out}, nil
		},
	})
}
