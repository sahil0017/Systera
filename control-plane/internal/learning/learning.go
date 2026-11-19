// Package learning implements Engineering Learning Mode (spec §2.7, §8.5): for any component
// of an architecture model it explains why the component exists and what problem it solves,
// shows alternatives and trade-offs, walks through failure scenarios and recovery, generates
// interview questions and hands-on exercises, connects the component to validated
// Engineering Pulse research, and lets the user challenge the design and receive a revised
// explanation.
//
// Lessons are derived deterministically from the canonical graph (the X-Ray component
// inspection), the ADR ledger and Pulse knowledge, so every statement can be traced to its
// basis. Lessons are teaching material about a PROPOSED design; they never upgrade the
// evidence class of what they explain.
package learning

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/systera/control-plane/internal/graph"
)

var (
	ErrInvalidInput = errors.New("invalid learning request")
)

// Disclaimer accompanies every lesson.
const Disclaimer = "Learning material explains a proposed design. It is not evidence that a real system is built this way; check the component's evidence state and claims for what is observed."

// MaxChallengeChars bounds a user challenge.
const MaxChallengeChars = 2000

// Explanation is one explained point together with what it was derived from.
type Explanation struct {
	Summary string   `json:"summary"`
	Details []string `json:"details"`
	Basis   []string `json:"basis"` // e.g. "graph: 3 upstream dependents", "guide: service"
}

// TradeOff is one alternative to the component's current design.
type TradeOff struct {
	Option       string `json:"option"`
	WhenToChoose string `json:"when_to_choose"`
	Pros         string `json:"pros"`
	Cons         string `json:"cons"`
	Source       string `json:"source"` // ADR | WEBSITE_ANALYSIS | GUIDE
	DecisionID   string `json:"decision_id,omitempty"`
}

// FailureWalkthrough explains one failure scenario end to end.
type FailureWalkthrough struct {
	Scenario  string `json:"scenario"`
	Severity  string `json:"severity"`
	Detection string `json:"detection"`
	Impact    string `json:"impact"`
	Recovery  string `json:"recovery"`
	Source    string `json:"source"` // GRAPH_THREAT | GRAPH_SLO | PROPERTY | STRUCTURAL | GUIDE
}

// InterviewQuestion is a question derived from the architecture with the points a strong
// answer covers.
type InterviewQuestion struct {
	Level            string   `json:"level"` // FOUNDATIONAL | INTERMEDIATE | ADVANCED
	Question         string   `json:"question"`
	GoodAnswersCover []string `json:"good_answers_cover"`
}

// Exercise is a hands-on implementation task.
type Exercise struct {
	Title              string   `json:"title"`
	Difficulty         string   `json:"difficulty"`
	Goal               string   `json:"goal"`
	Steps              []string `json:"steps"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
}

// ResearchLink connects a design decision to validated Engineering Pulse knowledge.
type ResearchLink struct {
	ProvenanceRef  string           `json:"provenance_ref"`
	TopicKey       string           `json:"topic_key"`
	Title          string           `json:"title"`
	Why            []string         `json:"why"`
	ConfidenceBand string           `json:"confidence_band"`
	Maturity       string           `json:"maturity"`
	Status         string           `json:"status"`
	Limitations    []string         `json:"limitations"`
	Sources        []ResearchSource `json:"sources"`
	// AdoptionNote repeats that popularity is never a selection criterion (§6.5).
	AdoptionNote string `json:"adoption_note"`
}

// ResearchSource is one source behind a research link.
type ResearchSource struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Source    string `json:"source"`
	IsPrimary bool   `json:"is_primary"`
}

// Lesson is the Learning Mode view of one component.
type Lesson struct {
	ModelID            string               `json:"model_id"`
	GraphVersion       int                  `json:"graph_version"`
	NodeID             string               `json:"node_id"`
	NodeKey            string               `json:"node_key"`
	NodeName           string               `json:"node_name"`
	NodeType           string               `json:"node_type"`
	Layer              string               `json:"layer"`
	EvidenceState      string               `json:"evidence_state"`
	Disclaimer         string               `json:"disclaimer"`
	WhyItExists        Explanation          `json:"why_it_exists"`
	ProblemSolved      Explanation          `json:"problem_solved"`
	Alternatives       []TradeOff           `json:"alternatives"`
	FailureScenarios   []FailureWalkthrough `json:"failure_scenarios"`
	InterviewQuestions []InterviewQuestion  `json:"interview_questions"`
	Exercises          []Exercise           `json:"exercises"`
	Research           []ResearchLink       `json:"research"`
	ResearchStatus     string               `json:"research_status"` // FOUND | NONE_FOUND | UNAVAILABLE
	Challenges         []Challenge          `json:"challenges"`
}

// RevisedExplanation answers a user challenge.
type RevisedExplanation struct {
	Concern           string         `json:"concern"` // classified concern
	Summary           string         `json:"summary"`
	Response          []string       `json:"response"`
	WhenYouAreRight   string         `json:"when_you_are_right"`
	ConsiderInstead   []TradeOff     `json:"consider_instead"`
	RelevantFailures  []string       `json:"relevant_failures"`
	EvidenceToDecide  []string       `json:"evidence_to_decide"`
	Research          []ResearchLink `json:"research"`
	DesignUnchanged   bool           `json:"design_unchanged"` // a challenge never mutates the graph by itself
	SuggestedNextStep string         `json:"suggested_next_step"`
}

// Challenge is one recorded challenge and its revised explanation.
type Challenge struct {
	ID           string             `json:"id"`
	ModelID      string             `json:"model_id"`
	NodeID       string             `json:"node_id"`
	GraphVersion int                `json:"graph_version"`
	Revision     int                `json:"revision"`
	Text         string             `json:"text"`
	Revised      RevisedExplanation `json:"revised"`
	CreatedBy    string             `json:"created_by"`
	CreatedAt    time.Time          `json:"created_at"`
}

// ComponentSummary is one row of the model-level learning overview.
type ComponentSummary struct {
	NodeID   string `json:"node_id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Layer    string `json:"layer"`
	OneLiner string `json:"one_liner"`
}

// Overview is the model-level Learning Mode entry point.
type Overview struct {
	ModelID            string              `json:"model_id"`
	GraphVersion       int                 `json:"graph_version"`
	Disclaimer         string              `json:"disclaimer"`
	Components         []ComponentSummary  `json:"components"`
	InterviewQuestions []InterviewQuestion `json:"interview_questions"`
	Exercises          []Exercise          `json:"exercises"`
}

func names(refs []graph.NodeRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Name)
	}
	return out
}

func edgeNames(refs []graph.EdgeNodeRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, fmt.Sprintf("%s (%s)", r.Node.Name, r.RelationType))
	}
	return out
}

func listOrNone(xs []string) string {
	if len(xs) == 0 {
		return "none recorded"
	}
	if len(xs) > 6 {
		return strings.Join(xs[:6], ", ") + fmt.Sprintf(" and %d more", len(xs)-6)
	}
	return strings.Join(xs, ", ")
}

// BuildLesson derives a lesson from a component inspection and any research links.
func BuildLesson(insp graph.ComponentInspection, research []ResearchLink, researchStatus string) Lesson {
	n := insp.Node.Node
	g := guideFor(n.Type)
	up, down := insp.Dependencies.Upstream, insp.Dependencies.Downstream

	why := Explanation{Summary: g.Purpose, Basis: []string{"guide: " + n.Type}}
	for _, r := range insp.Responsibilities {
		why.Details = append(why.Details, "Responsibility: "+r)
	}
	if len(why.Details) > 0 {
		why.Basis = append(why.Basis, fmt.Sprintf("graph: %d recorded responsibilities", len(insp.Responsibilities)))
	}
	if len(up) > 0 {
		why.Details = append(why.Details, fmt.Sprintf("%d component(s) depend on it: %s. Removing it would leave them without this capability.", len(up), listOrNone(edgeNames(up))))
		why.Basis = append(why.Basis, fmt.Sprintf("graph: %d upstream dependents", len(up)))
	}
	if len(insp.OwnedComponents) > 0 {
		why.Details = append(why.Details, "It is the owner of: "+listOrNone(names(insp.OwnedComponents))+".")
	}
	if len(why.Details) == 0 {
		why.Details = []string{"No responsibilities or dependents are recorded for this component yet; the explanation above is the general role of a " + n.Type + "."}
	}

	problem := Explanation{Summary: g.Problem, Basis: []string{"guide: " + n.Type}}
	do := insp.DataOwnership
	if len(do.Owns)+len(do.Writes) > 0 {
		problem.Details = append(problem.Details, "It keeps a single owner for writes to: "+listOrNone(append(names(do.Owns), names(do.Writes)...))+".")
		problem.Basis = append(problem.Basis, "graph: data ownership")
	}
	for _, it := range insp.Interfaces {
		detail := fmt.Sprintf("%s %s", strings.ToLower(it.Role), it.Target.Name)
		if it.Protocol != "" || it.SyncMode != "" {
			detail += fmt.Sprintf(" (%s)", strings.Trim(it.Protocol+" "+it.SyncMode, " "))
		}
		problem.Details = append(problem.Details, "Interface: it "+detail+".")
	}
	if len(insp.Interfaces) > 0 {
		problem.Basis = append(problem.Basis, fmt.Sprintf("graph: %d interfaces", len(insp.Interfaces)))
	}
	if len(down) > 0 {
		problem.Details = append(problem.Details, "To do its job it relies on: "+listOrNone(edgeNames(down))+".")
	}

	// Alternatives: recorded ADR / website-analysis alternatives first, then the guide.
	alts := []TradeOff{}
	seen := map[string]bool{}
	for _, a := range insp.Alternatives {
		key := strings.ToLower(a.Option)
		if a.Option == "" || seen[key] {
			continue
		}
		seen[key] = true
		when := a.RejectionReason
		if when == "" {
			when = a.Title
		}
		alts = append(alts, TradeOff{Option: a.Option, WhenToChoose: when, Pros: a.Pros, Cons: a.Cons, Source: a.Source, DecisionID: a.DecisionID})
	}
	for _, a := range g.Alternatives {
		if seen[strings.ToLower(a.Option)] {
			continue
		}
		a.Source = "GUIDE"
		alts = append(alts, a)
	}

	// Failure walkthroughs: graph-derived first (they are about this design), then the guide.
	fails := []FailureWalkthrough{}
	fseen := map[string]bool{}
	for _, f := range insp.FailureModes {
		if fseen[strings.ToLower(f.Title)] {
			continue
		}
		fseen[strings.ToLower(f.Title)] = true
		rec := f.Mitigation
		if rec == "" {
			rec = "No mitigation recorded yet. Decide one and record it as a graph property or ADR."
		}
		fails = append(fails, FailureWalkthrough{Scenario: f.Title, Severity: f.Severity, Detection: "See the linked SLOs and alerts for this component.",
			Impact: f.Description, Recovery: rec, Source: f.Source})
	}
	for _, f := range g.Failures {
		if fseen[strings.ToLower(f.Scenario)] {
			continue
		}
		f.Source = "GUIDE"
		fails = append(fails, f)
	}
	if len(up) > 0 && len(fails) > 0 {
		for i := range fails {
			if fails[i].Severity == "HIGH" && !strings.Contains(fails[i].Impact, "dependents") {
				fails[i].Impact += fmt.Sprintf(" In this design that affects its %d dependent(s): %s.", len(up), listOrNone(edgeNames(up)))
			}
		}
	}

	questions := make([]InterviewQuestion, 0, len(g.Questions)+3)
	for _, iq := range g.Questions {
		iq.Question = strings.ReplaceAll(iq.Question, "this "+n.Type, fmt.Sprintf("the %s %s", n.Name, n.Type))
		questions = append(questions, iq)
	}
	if len(up) > 0 {
		questions = append(questions, q("INTERMEDIATE",
			fmt.Sprintf("%s is down. What happens to %s, and how would you limit the impact?", n.Name, listOrNone(names(nodeRefs(up)))),
			"timeouts", "circuit breakers", "fallbacks or degraded mode", "blast radius"))
	}
	for _, d := range down {
		if d.SyncMode == "SYNC" {
			questions = append(questions, q("ADVANCED",
				fmt.Sprintf("%s calls %s synchronously. When would you make that interaction asynchronous, and what would it cost?", n.Name, d.Node.Name),
				"latency coupling", "availability coupling", "eventual consistency", "outbox / events"))
			break
		}
	}
	for _, a := range insp.Alternatives {
		if a.ChosenOption != "" && a.Option != "" {
			questions = append(questions, q("ADVANCED",
				fmt.Sprintf("The design chose %s over %s. Argue for the rejected option: under which constraints would it win?", a.ChosenOption, a.Option),
				"explicit constraints (scale, team size, cost...)", "the ADR's trade-offs", "evidence that would change the decision"))
			break
		}
	}

	exercises := append([]Exercise{}, g.Exercises...)
	for _, it := range insp.Interfaces {
		if it.Role == "PUBLISHES" && !hasExercise(exercises, "Transactional outbox publisher") {
			exercises = append(exercises, guides["topic"].Exercises[0])
		}
		if it.Role == "SUBSCRIBES" && !hasExercise(exercises, "Idempotent consumer with DLQ") {
			exercises = append(exercises, guides["queue"].Exercises[0])
		}
	}
	for _, d := range down {
		if d.Node.Type == "external_dependency" && !hasExercise(exercises, "Circuit breaker and reconciliation") {
			exercises = append(exercises, guides["external_dependency"].Exercises[0])
		}
	}
	if research == nil {
		research = []ResearchLink{}
	}

	return Lesson{
		ModelID: insp.ModelID, GraphVersion: insp.Version, NodeID: n.ID, NodeKey: n.Key, NodeName: n.Name, NodeType: n.Type,
		Layer: string(insp.Node.Layer), EvidenceState: n.EvidenceState, Disclaimer: Disclaimer,
		WhyItExists: why, ProblemSolved: problem, Alternatives: alts, FailureScenarios: fails,
		InterviewQuestions: questions, Exercises: exercises, Research: research, ResearchStatus: researchStatus,
		Challenges: []Challenge{},
	}
}

func nodeRefs(refs []graph.EdgeNodeRef) []graph.NodeRef {
	out := make([]graph.NodeRef, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Node)
	}
	return out
}

func hasExercise(xs []Exercise, title string) bool {
	for _, x := range xs {
		if x.Title == title {
			return true
		}
	}
	return false
}

// Concern categories a challenge is classified into.
const (
	ConcernAlternative = "ALTERNATIVE"
	ConcernComplexity  = "COMPLEXITY"
	ConcernCost        = "COST"
	ConcernScale       = "SCALE_PERFORMANCE"
	ConcernConsistency = "CONSISTENCY"
	ConcernReliability = "RELIABILITY"
	ConcernSecurity    = "SECURITY"
	ConcernGeneral     = "GENERAL"
)

var concernRules = []struct {
	concern string
	re      *regexp.Regexp
}{
	{ConcernSecurity, regexp.MustCompile(`(?i)\b(secur\w*|auth\w*|attack\w*|leak\w*|encrypt\w*|pii|privacy|permission\w*|vulnerab\w*)\b`)},
	{ConcernConsistency, regexp.MustCompile(`(?i)\b(consisten\w*|transaction\w*|stale|race|duplicat\w*|idempoten\w*|ordering|out of order)\b`)},
	{ConcernReliability, regexp.MustCompile(`(?i)\b(fail\w*|outage|down|single point|spof|availab\w*|resilien\w*|recover\w*|crash\w*)\b`)},
	{ConcernScale, regexp.MustCompile(`(?i)\b(scal\w*|performan\w*|latenc\w*|slow|throughput|load|traffic|bottleneck)\b`)},
	{ConcernCost, regexp.MustCompile(`(?i)\b(cost\w*|expensive|cheap\w*|budget|price|pricing|spend)\b`)},
	{ConcernComplexity, regexp.MustCompile(`(?i)\b(complex\w*|overkill|over-?engineer\w*|simpl\w*|unnecessary|too many|overhead|why do we need)\b`)},
}

var whyNot = regexp.MustCompile(`(?i)\b(why not|instead of|rather than|what about|use (a|an)?)\b`)

// NormalizeChallenge validates and trims a challenge.
func NormalizeChallenge(text string) (string, error) {
	t := strings.TrimSpace(text)
	if t == "" {
		return "", fmt.Errorf("%w: challenge text is required", ErrInvalidInput)
	}
	if utf8.RuneCountInString(t) > MaxChallengeChars {
		return "", fmt.Errorf("%w: challenge is at most %d characters", ErrInvalidInput, MaxChallengeChars)
	}
	return t, nil
}

// ClassifyConcern maps a challenge onto a concern category and any alternatives it names.
func ClassifyConcern(text string, alts []TradeOff) (string, []TradeOff) {
	lower := strings.ToLower(text)
	var named []TradeOff
	for _, a := range alts {
		for _, word := range significantWords(a.Option) {
			if strings.Contains(lower, word) {
				named = append(named, a)
				break
			}
		}
	}
	if len(named) > 0 && whyNot.MatchString(text) {
		return ConcernAlternative, named
	}
	for _, r := range concernRules {
		if r.re.MatchString(text) {
			return r.concern, named
		}
	}
	if len(named) > 0 {
		return ConcernAlternative, named
	}
	return ConcernGeneral, nil
}

var stop = map[string]bool{"inside": true, "with": true, "over": true, "service": true, "separate": true, "instead": true, "the": true, "and": true, "multiple": true, "behind": true, "adapter": true, "design": true}

func significantWords(option string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(option), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-')
	}) {
		if len(w) >= 4 && !stop[w] {
			out = append(out, w)
		}
	}
	return out
}

func failuresMatching(fails []FailureWalkthrough, concern string) []string {
	var out []string
	for _, f := range fails {
		text := f.Scenario + " " + f.Impact
		switch concern {
		case ConcernReliability:
			if f.Severity == "HIGH" || regexp.MustCompile(`(?i)outage|failure|down|crash`).MatchString(text) {
				out = append(out, f.Scenario+": "+f.Recovery)
			}
		case ConcernScale:
			if regexp.MustCompile(`(?i)overload|spike|saturat|backlog|hot|stampede|latency`).MatchString(text) {
				out = append(out, f.Scenario+": "+f.Recovery)
			}
		case ConcernConsistency:
			if regexp.MustCompile(`(?i)duplicat|stale|order|lost|diverge|inconsisten`).MatchString(text) {
				out = append(out, f.Scenario+": "+f.Recovery)
			}
		case ConcernSecurity:
			if regexp.MustCompile(`(?i)leak|abuse|unauthori|credential`).MatchString(text) {
				out = append(out, f.Scenario+": "+f.Recovery)
			}
		}
	}
	return out
}

// ReviseExplanation answers a challenge against the lesson. It explains the design from the
// angle the user raised, concedes when the alternative is the better choice, and names the
// evidence that would settle it. It never changes the design itself.
func ReviseExplanation(l Lesson, text string) RevisedExplanation {
	concern, named := ClassifyConcern(text, l.Alternatives)
	name := l.NodeName
	out := RevisedExplanation{Concern: concern, DesignUnchanged: true, Research: l.Research,
		SuggestedNextStep: "If the answer does not resolve your concern, generate counterfactual alternatives for this model or propose a change set; the reviewer will re-check it."}

	pick := func(match func(TradeOff) bool) []TradeOff {
		var xs []TradeOff
		for _, a := range l.Alternatives {
			if match(a) {
				xs = append(xs, a)
			}
		}
		return xs
	}
	mentions := func(re string) func(TradeOff) bool {
		r := regexp.MustCompile("(?i)" + re)
		return func(a TradeOff) bool { return r.MatchString(a.Option + " " + a.Pros + " " + a.WhenToChoose) }
	}

	switch concern {
	case ConcernAlternative:
		a := named[0]
		out.Summary = fmt.Sprintf("You are proposing %s instead of the current %s design.", a.Option, name)
		out.Response = []string{
			fmt.Sprintf("The current design is: %s", l.WhyItExists.Summary),
			fmt.Sprintf("%s gives you: %s", a.Option, orDash(a.Pros)),
			fmt.Sprintf("It costs you: %s", orDash(a.Cons)),
		}
		if a.Source == "ADR" {
			out.Response = append(out.Response, "This option was considered and rejected in a recorded ADR; the rejection reason is shown as when_to_choose.")
		}
		out.WhenYouAreRight = fmt.Sprintf("%s is the better choice when: %s", a.Option, orDash(a.WhenToChoose))
		out.ConsiderInstead = named
		out.EvidenceToDecide = []string{"Expected scale and growth for this component.", "Team size and ownership boundaries.", "Consistency requirements of the data it owns.", "Operational maturity (on-call, observability, deployment tooling)."}
	case ConcernComplexity:
		out.Summary = fmt.Sprintf("You think %s may be more than the problem needs.", name)
		out.Response = []string{fmt.Sprintf("It exists to solve this: %s", l.ProblemSolved.Summary)}
		out.Response = append(out.Response, l.WhyItExists.Details...)
		out.ConsiderInstead = pick(mentions(`monolith|inline|no cache|synchronous|single region|fewer|simple|in-process`))
		out.WhenYouAreRight = "If none of the dependents or failure scenarios above apply at your expected scale, the simpler option is the better choice: complexity should be earned by a measured need."
		out.EvidenceToDecide = []string{"Is the problem it solves already causing pain (latency, outages, coordination cost)?", "How many components really depend on it?", "What would removing it cost later if the need appears?"}
	case ConcernCost:
		out.Summary = fmt.Sprintf("You are concerned about what %s costs to run.", name)
		out.Response = []string{"Cost should be compared against the failure and scaling scenarios it prevents, not in isolation."}
		out.ConsiderInstead = pick(mentions(`monolith|serverless|in-process|no cache|single region|managed|cheap|pay-per-use`))
		out.WhenYouAreRight = "If traffic is low or spiky and the failure scenarios are tolerable, a cheaper option (consolidation, serverless or removing the component) is justified."
		out.EvidenceToDecide = []string{"Current and projected traffic.", "Cost per request of each option.", "The business cost of the failure scenarios this component prevents."}
	case ConcernScale:
		out.Summary = fmt.Sprintf("You are asking whether %s holds up under load.", name)
		out.RelevantFailures = failuresMatching(l.FailureScenarios, concern)
		out.Response = []string{"Scaling limits usually show up first as the saturation scenarios below; each has a known mitigation."}
		out.ConsiderInstead = pick(mentions(`scal|partition|shard|replica|cache|stream|independent`))
		out.WhenYouAreRight = "If measured or estimated peak load exceeds what the mitigations can absorb, change the design (partitioning, async processing or independent scaling) rather than tuning it."
		out.EvidenceToDecide = []string{"Peak requests per second and read/write ratio.", "p99 latency budget for the journeys that use it.", "A load test or a Digital Twin capacity estimate."}
	case ConcernConsistency:
		out.Summary = fmt.Sprintf("You are concerned about data correctness around %s.", name)
		out.RelevantFailures = failuresMatching(l.FailureScenarios, concern)
		out.Response = []string{"Correctness issues come from duplicates, ordering and lost updates. The design handles them with the mechanisms listed below; if a mechanism is missing, it is a real gap."}
		out.ConsiderInstead = pick(mentions(`relational|transaction|acid|synchronous|monolith`))
		out.WhenYouAreRight = "If the business cannot tolerate any window of inconsistency here, prefer a single transactional boundary over asynchronous propagation."
		out.EvidenceToDecide = []string{"Which invariants must hold at all times vs eventually?", "What is the cost of a duplicate or out-of-order update?"}
	case ConcernReliability:
		out.Summary = fmt.Sprintf("You are asking what happens when %s fails.", name)
		out.RelevantFailures = failuresMatching(l.FailureScenarios, concern)
		out.Response = []string{"Each failure scenario below has a detection signal and a recovery path. Review whether each recovery path exists in the design, not just on paper."}
		out.WhenYouAreRight = "If a HIGH-severity scenario has no recorded mitigation, the design has a genuine reliability gap and should be changed."
		out.EvidenceToDecide = []string{"Availability target (SLO) for the journeys that depend on it.", "Recovery time and data-loss objectives.", "Results of a failure drill or simulation."}
	case ConcernSecurity:
		out.Summary = fmt.Sprintf("You are raising a security concern about %s.", name)
		out.RelevantFailures = failuresMatching(l.FailureScenarios, concern)
		out.Response = []string{"Security concerns should be recorded as threats on the graph so a Red Team review checks the mitigation. Learning Mode does not judge whether a real system is vulnerable."}
		out.WhenYouAreRight = "If the component handles credentials, personal data or payments without an explicit trust boundary and control, treat the concern as a design defect."
		out.EvidenceToDecide = []string{"What data and credentials cross this component?", "Which authentication and authorization controls apply?", "Is there a recorded threat and mitigation?"}
	default:
		out.Summary = fmt.Sprintf("Here is %s explained again, step by step.", name)
		out.Response = append([]string{l.WhyItExists.Summary}, l.WhyItExists.Details...)
		out.Response = append(out.Response, "Problem it solves: "+l.ProblemSolved.Summary)
		out.WhenYouAreRight = "If the explanation does not match how you think the system should work, name the alternative you have in mind (for example \"why not a monolith?\") for a direct comparison."
		out.EvidenceToDecide = []string{"Which requirement does your concern relate to?"}
	}
	if out.ConsiderInstead == nil {
		out.ConsiderInstead = []TradeOff{}
	}
	if out.RelevantFailures == nil {
		out.RelevantFailures = []string{}
	}
	return out
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "not recorded"
	}
	return s
}

// skipOverviewTypes are structural or bookkeeping nodes not worth a lesson in the overview.
var skipOverviewTypes = map[string]bool{"test": true}

// BuildOverview summarises every component of a graph version for Learning Mode and adds
// system-level interview questions and exercises.
func BuildOverview(g graph.Graph) Overview {
	ov := Overview{ModelID: g.Version.ModelID, GraphVersion: g.Version.Number, Disclaimer: Disclaimer,
		Components: []ComponentSummary{}, InterviewQuestions: []InterviewQuestion{}, Exercises: []Exercise{}}
	types := map[string]int{}
	for _, n := range g.Nodes {
		if skipOverviewTypes[n.Type] {
			continue
		}
		types[n.Type]++
		purpose := guideFor(n.Type).Purpose
		if i := strings.Index(purpose, ":"); i > 0 && i < len(purpose)-1 {
			purpose = strings.TrimSpace(purpose[i+1:])
		}
		one := n.Description
		if strings.TrimSpace(one) == "" {
			one = purpose
		}
		ov.Components = append(ov.Components, ComponentSummary{NodeID: n.ID, Name: n.Name, Type: n.Type, Layer: string(graph.ResolveXRayLayer(n)), OneLiner: one})
	}
	sort.Slice(ov.Components, func(i, j int) bool {
		if ov.Components[i].Layer != ov.Components[j].Layer {
			return layerRank(ov.Components[i].Layer) < layerRank(ov.Components[j].Layer)
		}
		return ov.Components[i].Name < ov.Components[j].Name
	})
	ov.InterviewQuestions = append(ov.InterviewQuestions, guides["system"].Questions...)
	if types["service"] > 1 {
		ov.InterviewQuestions = append(ov.InterviewQuestions, q("ADVANCED",
			fmt.Sprintf("This design has %d services. How would you trace one user request across all of them and find where its latency goes?", types["service"]),
			"distributed tracing", "correlation IDs", "span attributes", "sampling"))
	}
	if types["topic"]+types["queue"] > 0 {
		ov.InterviewQuestions = append(ov.InterviewQuestions, q("ADVANCED",
			"Which parts of this design are eventually consistent, and how would a user notice?",
			"async boundaries", "read-your-writes", "UI feedback for pending state"))
		ov.Exercises = append(ov.Exercises, guides["topic"].Exercises[0])
	}
	if types["api"] > 0 {
		ov.Exercises = append(ov.Exercises, guides["api"].Exercises[0])
	}
	if types["database"] > 0 {
		ov.Exercises = append(ov.Exercises, guides["database"].Exercises[0])
	}
	if types["external_dependency"] > 0 {
		ov.Exercises = append(ov.Exercises, guides["external_dependency"].Exercises[0])
	}
	return ov
}

func layerRank(l string) int {
	for i, x := range graph.AllXRayLayers {
		if string(x) == l {
			return i
		}
	}
	return len(graph.AllXRayLayers)
}
