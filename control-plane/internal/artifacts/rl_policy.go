package artifacts

import (
	"math"
	"math/rand"
	"sync"
	"time"
)

// StrategyArm identifies a projection rendering/enrichment strategy selected by the RL policy.
type StrategyArm string

const (
	ArmConcise         StrategyArm = "concise"
	ArmStandard        StrategyArm = "standard"
	ArmDetailed        StrategyArm = "detailed_with_rationale"
	ArmLearningFocused StrategyArm = "interview_learning"
)

// DefaultArms lists the strategy arms available to the projection RL policy.
var DefaultArms = []StrategyArm{
	ArmConcise,
	ArmStandard,
	ArmDetailed,
	ArmLearningFocused,
}

// ArmStats tracks learned Q-value and pull counts for a single (projection_type, arm) pair.
type ArmStats struct {
	Arm         StrategyArm `json:"arm"`
	Pulls       int         `json:"pulls"`
	QValue      float64     `json:"q_value"`
	TotalReward float64     `json:"total_reward"`
}

// Selection records a non-deterministic RL policy decision for a projection run.
type Selection struct {
	ProjectionType ProjectionType `json:"projection_type"`
	Arm            StrategyArm    `json:"arm"`
	Explored       bool           `json:"explored"`
	PolicyVersion  int            `json:"policy_version"`
}

// RewardSignal captures feedback from evaluators, simulations, or users (§23.12).
type RewardSignal struct {
	ProjectionType ProjectionType `json:"projection_type"`
	Arm            StrategyArm    `json:"arm"`
	// Reward in [0.0, 1.0], e.g. weighted evaluator score + user acceptance.
	Reward float64 `json:"reward"`
}

// RLPolicy implements a non-deterministic Epsilon-Greedy + Softmax (Boltzmann)
// reinforcement learning policy over projection strategies.
type RLPolicy struct {
	mu            sync.Mutex
	rng           *rand.Rand
	epsilon       float64
	temperature   float64
	learningRate  float64
	policyVersion int
	qTable        map[ProjectionType]map[StrategyArm]*ArmStats
}

// NewRLPolicy creates a stochastic RL policy for projection strategy selection.
//   - epsilon: exploration probability in [0, 1]
//   - temperature: softmax temperature (> 0; higher = more non-deterministic exploration)
//   - learningRate: step size alpha in (0, 1] for incremental Q-value updates
func NewRLPolicy(epsilon, temperature, learningRate float64) *RLPolicy {
	if epsilon < 0 {
		epsilon = 0
	}
	if epsilon > 1 {
		epsilon = 1
	}
	if temperature <= 0 {
		temperature = 0.25
	}
	if learningRate <= 0 || learningRate > 1 {
		learningRate = 0.2
	}
	return &RLPolicy{
		rng:           rand.New(rand.NewSource(time.Now().UnixNano())),
		epsilon:       epsilon,
		temperature:   temperature,
		learningRate:  learningRate,
		policyVersion: 1,
		qTable:        make(map[ProjectionType]map[StrategyArm]*ArmStats),
	}
}

func (p *RLPolicy) ensureType(pt ProjectionType) map[StrategyArm]*ArmStats {
	m, ok := p.qTable[pt]
	if !ok {
		m = make(map[StrategyArm]*ArmStats, len(DefaultArms))
		for _, arm := range DefaultArms {
			m[arm] = &ArmStats{Arm: arm, QValue: 0.5} // optimistic initialization
		}
		p.qTable[pt] = m
	}
	return m
}

// SelectArm non-deterministically selects a StrategyArm for the given ProjectionType.
// With probability epsilon it samples via Boltzmann/Softmax over Q-values; otherwise it
// exploits the highest-Q arm (breaking ties randomly).
func (p *RLPolicy) SelectArm(pt ProjectionType) Selection {
	p.mu.Lock()
	defer p.mu.Unlock()

	arms := p.ensureType(pt)

	// Exploration branch: stochastic Softmax sampling.
	if p.rng.Float64() < p.epsilon {
		arm := p.sampleSoftmax(arms)
		return Selection{
			ProjectionType: pt,
			Arm:            arm,
			Explored:       true,
			PolicyVersion:  p.policyVersion,
		}
	}

	// Exploitation branch: argmax Q(s, a) with random tie-breaking.
	bestQ := -math.MaxFloat64
	var tied []StrategyArm
	for _, arm := range DefaultArms {
		st := arms[arm]
		if st.QValue > bestQ+1e-9 {
			bestQ = st.QValue
			tied = []StrategyArm{arm}
		} else if math.Abs(st.QValue-bestQ) <= 1e-9 {
			tied = append(tied, arm)
		}
	}
	chosen := tied[p.rng.Intn(len(tied))]
	return Selection{
		ProjectionType: pt,
		Arm:            chosen,
		Explored:       false,
		PolicyVersion:  p.policyVersion,
	}
}

func (p *RLPolicy) sampleSoftmax(arms map[StrategyArm]*ArmStats) StrategyArm {
	weights := make([]float64, len(DefaultArms))
	maxQ := -math.MaxFloat64
	for _, arm := range DefaultArms {
		if q := arms[arm].QValue; q > maxQ {
			maxQ = q
		}
	}
	var sum float64
	for i, arm := range DefaultArms {
		w := math.Exp((arms[arm].QValue - maxQ) / p.temperature)
		weights[i] = w
		sum += w
	}
	r := p.rng.Float64() * sum
	var acc float64
	for i, w := range weights {
		acc += w
		if r <= acc {
			return DefaultArms[i]
		}
	}
	return DefaultArms[len(DefaultArms)-1]
}

// RecordReward updates Q(projection_type, arm) using incremental RL update:
// Q <- Q + alpha * (reward - Q).
func (p *RLPolicy) RecordReward(sig RewardSignal) ArmStats {
	p.mu.Lock()
	defer p.mu.Unlock()

	reward := sig.Reward
	if reward < 0 {
		reward = 0
	}
	if reward > 1 {
		reward = 1
	}

	arms := p.ensureType(sig.ProjectionType)
	st, ok := arms[sig.Arm]
	if !ok {
		st = &ArmStats{Arm: sig.Arm, QValue: 0.5}
		arms[sig.Arm] = st
	}
	st.Pulls++
	st.TotalReward += reward
	st.QValue = st.QValue + p.learningRate*(reward-st.QValue)
	p.policyVersion++
	return *st
}

// Snapshot returns a copy of learned arm statistics for a projection type.
func (p *RLPolicy) Snapshot(pt ProjectionType) []ArmStats {
	p.mu.Lock()
	defer p.mu.Unlock()

	arms := p.ensureType(pt)
	out := make([]ArmStats, 0, len(DefaultArms))
	for _, arm := range DefaultArms {
		out = append(out, *arms[arm])
	}
	return out
}
