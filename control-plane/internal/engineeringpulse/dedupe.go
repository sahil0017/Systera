package engineeringpulse

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
	"math/bits"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// NearDuplicateMaxDistance is the largest SimHash Hamming distance treated as a near-duplicate.
// Feed entries are short (tens of words), so the classic k=3 used for whole web pages is too
// strict. Measured on bigram SimHash: a copy with a retitled header and a trailing note is ~6
// bits away, a different post on the same topic ~30, unrelated text ~36.
const NearDuplicateMaxDistance = 10

// minNearDupTokens: very short texts (bare titles) collide too easily for SimHash.
const minNearDupTokens = 8

var trackingParams = map[string]bool{
	"fbclid": true, "gclid": true, "mc_cid": true, "mc_eid": true, "ref": true, "ref_src": true,
	"source": true, "cmpid": true, "igshid": true, "spm": true,
}

// CanonicalURL normalizes a URL so mirrors of one document compare equal: lower-case
// scheme/host, no "www." / "m." / "amp." prefix, no default port, no fragment, no tracking
// parameters, sorted query, no trailing slash, and AMP path suffixes removed.
func CanonicalURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimSpace(raw))
	}
	host := strings.ToLower(u.Hostname())
	for _, p := range []string{"www.", "m.", "amp."} {
		host = strings.TrimPrefix(host, p)
	}
	if port := u.Port(); port != "" && port != "80" && port != "443" {
		host += ":" + port
	}
	path := u.EscapedPath()
	for _, suf := range []string{"/amp", "/amp/", ".amp"} {
		path = strings.TrimSuffix(path, suf)
	}
	path = strings.TrimRight(path, "/")
	q := u.Query()
	for k := range q {
		if strings.HasPrefix(strings.ToLower(k), "utm_") || trackingParams[strings.ToLower(k)] {
			q.Del(k)
		}
	}
	out := "https://" + host + path
	if enc := q.Encode(); enc != "" { // Encode sorts by key
		out += "?" + enc
	}
	return out
}

var nonWord = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// NormTitle lower-cases, strips punctuation and common repost prefixes.
func NormTitle(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	for _, p := range []string{"[repost]", "repost:", "re:", "fwd:", "[video]", "[podcast]"} {
		t = strings.TrimPrefix(strings.TrimSpace(t), p)
	}
	return strings.TrimSpace(nonWord.ReplaceAllString(t, " "))
}

// ContentHash is the exact-duplicate fingerprint: normalized title + body.
func ContentHash(title, body string) string {
	sum := sha256.Sum256([]byte(NormTitle(title) + "\n" + strings.Join(tokens(body), " ")))
	return hex.EncodeToString(sum[:])
}

func tokens(s string) []string {
	f := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	return f
}

// SimHash computes a 64-bit SimHash over word bigrams (Charikar). Near-duplicate texts have
// a small Hamming distance.
func SimHash(text string) (uint64, int) {
	toks := tokens(text)
	if len(toks) == 0 {
		return 0, 0
	}
	var v [64]int
	add := func(s string) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(s))
		x := h.Sum64()
		for i := 0; i < 64; i++ {
			if x&(1<<uint(i)) != 0 {
				v[i]++
			} else {
				v[i]--
			}
		}
	}
	if len(toks) < 2 {
		add(toks[0])
	} else {
		for i := 0; i+2 <= len(toks); i++ {
			add(toks[i] + " " + toks[i+1])
		}
	}
	var out uint64
	for i := 0; i < 64; i++ {
		if v[i] > 0 {
			out |= 1 << uint(i)
		}
	}
	return out, len(toks)
}

// Hamming returns the number of differing bits.
func Hamming(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// DedupCandidate is an existing item considered as the canonical for a new one.
type DedupCandidate struct {
	ID              string
	CanonicalID     string // the candidate's own canonical (== ID when it is canonical)
	SourceID        string
	IsPrimarySource bool
	CanonicalURL    string
	NormTitle       string
	ContentHash     string
	SeriesKey       string
	VersionLabel    string
	SimHash         uint64
	Tokens          int
	PublishedUnix   int64
}

// DedupDecision says how a new item relates to existing ones.
type DedupDecision struct {
	Kind        string  // "" = new canonical item
	CanonicalID string  // the canonical item it duplicates
	Score       float64 // similarity in [0,1]
	// Promote: the new item should become the canonical instead (newer version, or a
	// primary source replacing a non-primary copy); the old canonical is re-pointed to it.
	Promote bool
}

// DecideDuplicate classifies a new item against candidates, strongest signal first:
// exact content, same canonical URL (mirror), same series (version), same title from another
// source (repost), then SimHash near-duplicate.
func DecideDuplicate(n DedupCandidate, cands []DedupCandidate) DedupDecision {
	root := func(c DedupCandidate) string {
		if c.CanonicalID != "" {
			return c.CanonicalID
		}
		return c.ID
	}
	byRoot := map[string]DedupCandidate{}
	for _, c := range cands {
		if c.CanonicalID == "" || c.CanonicalID == c.ID {
			byRoot[c.ID] = c
		}
	}
	primaryPromote := func(canonID string) bool {
		c, ok := byRoot[canonID]
		return ok && n.IsPrimarySource && !c.IsPrimarySource
	}
	for _, c := range cands {
		if c.ContentHash == n.ContentHash {
			r := root(c)
			return DedupDecision{Kind: DupExact, CanonicalID: r, Score: 1, Promote: primaryPromote(r)}
		}
	}
	for _, c := range cands {
		if n.CanonicalURL != "" && c.CanonicalURL == n.CanonicalURL {
			r := root(c)
			return DedupDecision{Kind: DupMirror, CanonicalID: r, Score: 1, Promote: primaryPromote(r)}
		}
	}
	if n.SeriesKey != "" {
		for _, c := range cands {
			if c.SeriesKey == n.SeriesKey {
				r := root(c)
				canon := byRoot[r]
				newer := compareVersion(n.VersionLabel, canon.VersionLabel) > 0 ||
					(n.VersionLabel == canon.VersionLabel && n.PublishedUnix > canon.PublishedUnix)
				return DedupDecision{Kind: DupVersion, CanonicalID: r, Score: 1, Promote: newer}
			}
		}
	}
	for _, c := range cands {
		if n.NormTitle != "" && c.NormTitle == n.NormTitle && c.SourceID != n.SourceID && len(strings.Fields(n.NormTitle)) >= 3 {
			r := root(c)
			return DedupDecision{Kind: DupRepost, CanonicalID: r, Score: 0.95, Promote: primaryPromote(r)}
		}
	}
	if n.Tokens >= minNearDupTokens {
		best, bestD := "", 65
		for _, c := range cands {
			if c.Tokens < minNearDupTokens {
				continue
			}
			if d := Hamming(n.SimHash, c.SimHash); d < bestD {
				best, bestD = root(c), d
			}
		}
		if best != "" && bestD <= NearDuplicateMaxDistance {
			return DedupDecision{Kind: DupNearDuplicate, CanonicalID: best, Score: 1 - float64(bestD)/64, Promote: primaryPromote(best)}
		}
	}
	return DedupDecision{}
}

// compareVersion orders labels such as "v2" / "v10" / "1.4.0" numerically.
func compareVersion(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

var digitsRe = regexp.MustCompile(`\d+`)

func versionParts(s string) []int {
	var out []int
	for _, d := range digitsRe.FindAllString(s, 8) {
		n := 0
		for _, r := range d {
			n = n*10 + int(r-'0')
			if n > 1<<30 {
				break
			}
		}
		out = append(out, n)
	}
	return out
}

func sortedUnique(in []string) []string {
	m := map[string]bool{}
	for _, s := range in {
		if s != "" {
			m[s] = true
		}
	}
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
