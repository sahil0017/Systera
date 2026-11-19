package engineeringpulse

import (
	"regexp"
	"sort"
	"strings"
)

// MinTopicConfidence is the lowest classification confidence that is stored.
const MinTopicConfidence = 0.3

// Classifier assigns taxonomy topics, a maturity and an engineering-significance score to an
// item. The default is rule-based (keyword matching on word boundaries); a model-backed
// classifier can implement the same interface.
type Classifier interface {
	Classify(in ClassifyInput) Classification
}

// ClassifyInput is the text and context of one item.
type ClassifyInput struct {
	Title        string
	Summary      string
	Content      string
	SourceType   string
	VersionLabel string
	Hints        map[string]any
}

// Classification is the result for one item.
type Classification struct {
	Topics       []TopicMatch
	Maturity     string
	Significance float64
	Signals      []string
}

// TopicMatch is one topic assignment.
type TopicMatch struct {
	TopicKey     string
	TopicID      string
	Facet        string
	Confidence   float64
	MatchedTerms []string
}

// RuleClassifier matches taxonomy keywords. Title matches weigh twice body matches.
type RuleClassifier struct {
	topics []compiledTopic
	parent map[string]string // topic key -> parent key
	byKey  map[string]Topic
}

type compiledTopic struct {
	Topic
	res []*regexp.Regexp
}

// NewRuleClassifier compiles a taxonomy.
func NewRuleClassifier(topics []Topic) *RuleClassifier {
	c := &RuleClassifier{parent: map[string]string{}, byKey: map[string]Topic{}}
	for _, t := range topics {
		ct := compiledTopic{Topic: t}
		for _, kw := range t.Keywords {
			ct.res = append(ct.res, keywordRe(kw))
		}
		c.topics = append(c.topics, ct)
		c.parent[t.Key] = t.ParentKey
		c.byKey[t.Key] = t
	}
	return c
}

func keywordRe(kw string) *regexp.Regexp {
	q := regexp.QuoteMeta(strings.ToLower(strings.TrimSpace(kw)))
	q = strings.ReplaceAll(q, `\ `, `[\s-]+`)
	q = strings.ReplaceAll(q, " ", `[\s-]+`)
	return regexp.MustCompile(`(?i)(^|[^\p{L}\p{N}])` + q + `(e?s)?([^\p{L}\p{N}]|$)`)
}

var (
	deprecatedRe   = regexp.MustCompile(`(?i)\b(deprecated|deprecation|end[\s-]of[\s-]life|eol|sunset(ting)?|no longer (supported|maintained))\b`)
	experimentalRe = regexp.MustCompile(`(?i)\b(experimental|alpha|tech(nical)? preview|developer preview|proposal|prototype|draft|research preview)\b`)
	emergingRe     = regexp.MustCompile(`(?i)\b(beta|release candidate|rc\d*|preview|incubat(ing|ion)|early access)\b`)
	mainstreamRe   = regexp.MustCompile(`(?i)\b(generally available|general availability|\bga\b|stable release|lts|long[\s-]term support|production[\s-]ready)\b`)

	significanceSignals = []struct {
		name string
		re   *regexp.Regexp
	}{
		{"breaking_change", regexp.MustCompile(`(?i)\b(breaking change|backwards?[\s-]incompatible|migration guide|migrat(e|ion))\b`)},
		{"security", regexp.MustCompile(`(?i)\b(security|vulnerabilit(y|ies)|cve-\d{4}-\d+|exploit|patch(ed)? .*(flaw|issue))\b`)},
		{"performance", regexp.MustCompile(`(?i)\b(performance|latency|throughput|benchmark(s|ed)?|faster|p99|tail latency)\b`)},
		{"architecture", regexp.MustCompile(`(?i)\b(architecture|design|scalab(le|ility)|consistency|fault[\s-]toleran(t|ce)|postmortem|incident|availability)\b`)},
	}
	majorVersionRe = regexp.MustCompile(`^v?(\d+)\.0(\.0)?$`)
)

var sourceTypeBase = map[string]float64{
	"RFC": 0.6, "PAPER": 0.55, "PRIMARY_DOC": 0.5, "ENGINEERING_BLOG": 0.5, "RELEASE_NOTES": 0.45, "COMMUNITY": 0.3,
}

// Classify implements Classifier.
func (c *RuleClassifier) Classify(in ClassifyInput) Classification {
	title := in.Title
	body := in.Summary + "\n" + truncateRunes(in.Content, 8000)
	direct := map[string]TopicMatch{}
	for _, t := range c.topics {
		weight := 0.0
		var terms []string
		for i, re := range t.res {
			switch {
			case re.MatchString(title):
				weight += 2
				terms = append(terms, t.Keywords[i])
			case re.MatchString(body):
				weight++
				terms = append(terms, t.Keywords[i])
			}
		}
		if weight == 0 {
			continue
		}
		conf := weight / 3
		if conf > 1 {
			conf = 1
		}
		if conf >= MinTopicConfidence {
			direct[t.Key] = TopicMatch{TopicKey: t.Key, TopicID: t.ID, Facet: t.Facet, Confidence: round3(conf), MatchedTerms: sortedUnique(terms)}
		}
	}
	// A technology or pattern match implies its domain (at reduced confidence).
	out := map[string]TopicMatch{}
	for k, m := range direct {
		out[k] = m
		for p := c.parent[k]; p != ""; p = c.parent[p] {
			pc := round3(m.Confidence * 0.8)
			if cur, ok := out[p]; ok && cur.Confidence >= pc {
				continue
			}
			if d, ok := direct[p]; ok && d.Confidence >= pc {
				continue
			}
			pt := c.byKey[p]
			out[p] = TopicMatch{TopicKey: p, TopicID: pt.ID, Facet: pt.Facet, Confidence: pc, MatchedTerms: m.MatchedTerms}
		}
	}
	res := Classification{Maturity: inferMaturity(in)}
	hasPattern := false
	for _, m := range out {
		res.Topics = append(res.Topics, m)
		if m.Facet == "PATTERN" {
			hasPattern = true
		}
	}
	sort.Slice(res.Topics, func(i, j int) bool {
		if res.Topics[i].Confidence != res.Topics[j].Confidence {
			return res.Topics[i].Confidence > res.Topics[j].Confidence
		}
		return res.Topics[i].TopicKey < res.Topics[j].TopicKey
	})
	res.Significance, res.Signals = significance(in, hasPattern)
	return res
}

func inferMaturity(in ClassifyInput) string {
	text := in.Title + " " + in.Summary + " " + truncateRunes(in.Content, 4000)
	pre, _ := in.Hints["prerelease"].(bool)
	switch {
	case deprecatedRe.MatchString(text):
		return MaturityDeprecated
	case experimentalRe.MatchString(text) || in.SourceType == "PAPER":
		return MaturityExperimental
	case pre || emergingRe.MatchString(in.Title+" "+in.VersionLabel):
		return MaturityEmerging
	case mainstreamRe.MatchString(text) || in.SourceType == "RELEASE_NOTES" || in.SourceType == "RFC":
		return MaturityMainstream
	}
	return MaturityEmerging
}

func significance(in ClassifyInput, hasPattern bool) (float64, []string) {
	s, ok := sourceTypeBase[in.SourceType]
	if !ok {
		s = 0.4
	}
	text := in.Title + " " + in.Summary + " " + truncateRunes(in.Content, 8000)
	var sig []string
	for _, x := range significanceSignals {
		if x.re.MatchString(text) {
			s += 0.1
			sig = append(sig, x.name)
		}
	}
	if majorVersionRe.MatchString(in.VersionLabel) {
		s += 0.1
		sig = append(sig, "major_version")
	}
	if hasPattern {
		s += 0.1
		sig = append(sig, "architecture_pattern")
	}
	if s > 1 {
		s = 1
	}
	return round3(s), sig
}

func round3(v float64) float64 { return float64(int64(v*1000+0.5)) / 1000 }
