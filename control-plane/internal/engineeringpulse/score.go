package engineeringpulse

import "math"

// Score weights (§2.2). The same formula is a generated column in trend_scores.
const (
	WeightPopularity   = 0.25
	WeightGrowth       = 0.25
	WeightSignificance = 0.20
	WeightReliability  = 0.20
	WeightNovelty      = 0.10
)

// ScoreInputs are the raw signals for one topic over a scoring window.
type ScoreInputs struct {
	WindowDays         int     `json:"window_days"`
	Mentions           int     `json:"mentions"`          // canonical items + mirrors/reposts in the window
	PrevMentions       int     `json:"prev_mentions"`     // same, in the preceding window
	BaselineMentions   int     `json:"baseline_mentions"` // same, in the BaselineWindows windows before that
	BaselineWindows    int     `json:"baseline_windows"`
	Engagement         int64   `json:"engagement"`       // reactions / comments reported by sources
	TopSignificance    float64 `json:"top_significance"` // mean significance (0-1) of the top canonical items
	AvgReliability     float64 `json:"avg_reliability"`  // mean source reliability (0-1)
	PrimaryShare       float64 `json:"primary_share"`    // share of canonical items from primary sources
	DistinctSources    int     `json:"distinct_sources"`
	DaysSinceFirstSeen float64 `json:"days_since_first_seen"`
}

// Components are the five normalized sub-scores, each 0-100.
type Components struct {
	Popularity   float64 `json:"popularity"`
	Growth       float64 `json:"growth"`
	Significance float64 `json:"significance"`
	Reliability  float64 `json:"reliability"`
	Novelty      float64 `json:"novelty"`
}

// Total applies Score = 0.25P + 0.25G + 0.20E + 0.20R + 0.10N.
func (c Components) Total() float64 {
	return round2(WeightPopularity*c.Popularity + WeightGrowth*c.Growth + WeightSignificance*c.Significance +
		WeightReliability*c.Reliability + WeightNovelty*c.Novelty)
}

// ComputeComponents normalizes raw signals to 0-100:
//
//	P popularity   = 100 * (1 - e^(-(mentions + 0.5*ln(1+engagement)) / 8))   saturating attention
//	G growth       = 50 + 50 * tanh(ln((mentions+1) / (prev+1)))             50 = flat, >50 rising
//	E significance = 100 * mean significance of the top items
//	R reliability  = 100 * (0.5*avg source reliability + 0.3*primary share + 0.2*source diversity)
//	N novelty      = 100 * (0.5*e^(-age/120d) + 0.5*mentions/(mentions + baseline per window))
func ComputeComponents(in ScoreInputs) Components {
	attention := float64(in.Mentions) + 0.5*math.Log1p(float64(max64(in.Engagement, 0)))
	p := 100 * (1 - math.Exp(-attention/8))
	g := 50 + 50*math.Tanh(math.Log(float64(in.Mentions+1)/float64(in.PrevMentions+1)))
	e := 100 * clamp01(in.TopSignificance)
	diversity := 0.0
	if in.DistinctSources > 1 {
		diversity = math.Min(1, float64(in.DistinctSources-1)/3)
	}
	r := 100 * (0.5*clamp01(in.AvgReliability) + 0.3*clamp01(in.PrimaryShare) + 0.2*diversity)
	age := in.DaysSinceFirstSeen
	if age < 0 {
		age = 0
	}
	perWindow := 0.0
	if in.BaselineWindows > 0 {
		perWindow = float64(in.BaselineMentions) / float64(in.BaselineWindows)
	}
	burst := 1.0
	if in.Mentions+int(perWindow) > 0 {
		burst = float64(in.Mentions) / (float64(in.Mentions) + perWindow)
	}
	n := 100 * (0.5*math.Exp(-age/120) + 0.5*burst)
	return Components{
		Popularity: clampScore(p), Growth: clampScore(g), Significance: clampScore(e),
		Reliability: clampScore(r), Novelty: clampScore(n),
	}
}

func clampScore(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return round2(v)
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
