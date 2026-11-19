package engineeringpulse

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MaxResponseBytes caps any single source response.
const MaxResponseBytes = 5 << 20

// DefaultUserAgent identifies the crawler generically (no personal details).
const DefaultUserAgent = "SysteraEngineeringPulse/1.0 (feed reader; respects source terms and rate limits)"

// FetchResult is one conditional GET of a source feed.
type FetchResult struct {
	RequestedURL string
	Status       int
	NotModified  bool
	ContentType  string
	Bytes        int64
	ETag         string
	LastModified string
	RetryAfter   time.Duration
	Items        []RawItem
}

// Fetcher performs source fetches. Redirects may not leave the feed's host, bodies are
// size-capped, and conditional requests (ETag / Last-Modified) are always sent.
type Fetcher struct {
	Client      *http.Client
	UserAgent   string
	GitHubToken string
}

// NewFetcher returns a Fetcher with a 30s timeout and a same-host redirect policy.
func NewFetcher(userAgent, githubToken string) *Fetcher {
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	c := &http.Client{Timeout: 30 * time.Second}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if !strings.EqualFold(req.URL.Hostname(), via[0].URL.Hostname()) {
			return ErrDisallowedRedirect
		}
		return nil
	}
	return &Fetcher{Client: c, UserAgent: userAgent, GitHubToken: githubToken}
}

// Fetch retrieves and parses one source.
func (f *Fetcher) Fetch(ctx context.Context, src Source) (FetchResult, error) {
	res := FetchResult{RequestedURL: src.FeedURL}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.FeedURL, nil)
	if err != nil {
		return res, err
	}
	req.Header.Set("User-Agent", f.UserAgent)
	if src.ETag != "" {
		req.Header.Set("If-None-Match", src.ETag)
	}
	if src.LastModified != "" {
		req.Header.Set("If-Modified-Since", src.LastModified)
	}
	switch src.Connector {
	case ConnectorGitHubReleases:
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if f.GitHubToken != "" {
			req.Header.Set("Authorization", "Bearer "+f.GitHubToken)
		}
	default:
		req.Header.Set("Accept", "application/atom+xml, application/rss+xml, application/xml;q=0.9, text/xml;q=0.8")
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	res.ContentType = resp.Header.Get("Content-Type")
	res.ETag = resp.Header.Get("ETag")
	res.LastModified = resp.Header.Get("Last-Modified")
	res.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))

	switch {
	case resp.StatusCode == http.StatusNotModified:
		res.NotModified = true
		res.ETag, res.LastModified = src.ETag, src.LastModified
		return res, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable ||
		(resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0"):
		if res.RetryAfter == 0 {
			if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
				res.RetryAfter = time.Until(time.Unix(reset, 0))
			}
		}
		return res, fmt.Errorf("%w: HTTP %d", ErrRateLimited, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return res, fmt.Errorf("%w: HTTP %d", ErrUnsupportedResponse, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return res, err
	}
	res.Bytes = int64(len(body))
	if len(body) > MaxResponseBytes {
		return res, ErrResponseTooLarge
	}
	switch src.Connector {
	case ConnectorGitHubReleases:
		res.Items, err = ParseGitHubReleases(body, githubRepo(src))
	case ConnectorArXiv:
		res.Items, err = ParseArXiv(body)
	case ConnectorRSS:
		res.Items, err = ParseFeed(body)
	default:
		err = fmt.Errorf("%w: connector %q", ErrUnsupportedResponse, src.Connector)
	}
	return res, err
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

func githubRepo(src Source) string {
	if r, ok := src.Config["repo"].(string); ok && r != "" {
		return r
	}
	u, err := url.Parse(src.FeedURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "repos" {
		return parts[1] + "/" + parts[2]
	}
	return ""
}

// ---------------------------------------------------------------------------
// RSS 2.0 / Atom
// ---------------------------------------------------------------------------

type rssDoc struct {
	XMLName xml.Name `xml:"rss"`
	Channel struct {
		Items []struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			GUID        string `xml:"guid"`
			Description string `xml:"description"`
			Encoded     string `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
			PubDate     string `xml:"pubDate"`
			Author      string `xml:"author"`
			Creator     string `xml:"http://purl.org/dc/elements/1.1/ creator"`
			Comments    string `xml:"http://purl.org/rss/1.0/modules/slash/ comments"`
		} `xml:"item"`
	} `xml:"channel"`
}

type atomDoc struct {
	XMLName xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID    string `xml:"id"`
	Title string `xml:"title"`
	Links []struct {
		Href string `xml:"href,attr"`
		Rel  string `xml:"rel,attr"`
		Type string `xml:"type,attr"`
	} `xml:"link"`
	Summary   string `xml:"summary"`
	Content   string `xml:"content"`
	Published string `xml:"published"`
	Updated   string `xml:"updated"`
	Authors   []struct {
		Name string `xml:"name"`
	} `xml:"author"`
}

func (e atomEntry) link() string {
	for _, l := range e.Links {
		if l.Rel == "" || l.Rel == "alternate" {
			if l.Type == "" || strings.Contains(l.Type, "html") {
				return l.Href
			}
		}
	}
	if len(e.Links) > 0 {
		return e.Links[0].Href
	}
	return ""
}

// ParseFeed parses RSS 2.0 or Atom.
func ParseFeed(body []byte) ([]RawItem, error) {
	root, err := rootElement(body)
	if err != nil {
		return nil, err
	}
	switch root {
	case "rss":
		var d rssDoc
		if err := xml.Unmarshal(body, &d); err != nil {
			return nil, fmt.Errorf("%w: rss: %v", ErrUnsupportedResponse, err)
		}
		out := make([]RawItem, 0, len(d.Channel.Items))
		for _, it := range d.Channel.Items {
			id := strings.TrimSpace(it.GUID)
			if id == "" {
				id = strings.TrimSpace(it.Link)
			}
			title := cleanText(it.Title)
			if id == "" || title == "" {
				continue
			}
			ri := RawItem{
				ExternalID: id, URL: strings.TrimSpace(it.Link), Title: title,
				Summary: cleanText(it.Description), Content: cleanText(it.Encoded),
				PublishedAt: parseTime(it.PubDate),
			}
			if a := firstNonEmpty(it.Creator, it.Author); a != "" {
				ri.Authors = []string{cleanText(a)}
			}
			if n, err := strconv.Atoi(strings.TrimSpace(it.Comments)); err == nil && n > 0 {
				ri.Engagement = n
			}
			if ri.URL == "" {
				ri.URL = id
			}
			out = append(out, ri)
		}
		return out, nil
	case "feed":
		var d atomDoc
		if err := xml.Unmarshal(body, &d); err != nil {
			return nil, fmt.Errorf("%w: atom: %v", ErrUnsupportedResponse, err)
		}
		out := make([]RawItem, 0, len(d.Entries))
		for _, e := range d.Entries {
			ri := atomItem(e)
			if ri.ExternalID == "" || ri.Title == "" {
				continue
			}
			out = append(out, ri)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%w: root element %q", ErrUnsupportedResponse, root)
}

func atomItem(e atomEntry) RawItem {
	ri := RawItem{
		ExternalID: strings.TrimSpace(e.ID), URL: strings.TrimSpace(e.link()), Title: cleanText(e.Title),
		Summary: cleanText(e.Summary), Content: cleanText(e.Content),
		PublishedAt: parseTime(firstNonEmpty(e.Published, e.Updated)),
	}
	for _, a := range e.Authors {
		if n := cleanText(a.Name); n != "" {
			ri.Authors = append(ri.Authors, n)
		}
	}
	if ri.ExternalID == "" {
		ri.ExternalID = ri.URL
	}
	if ri.URL == "" {
		ri.URL = ri.ExternalID
	}
	return ri
}

var arxivIDRe = regexp.MustCompile(`arxiv\.org/abs/([^v\s]+?)(v(\d+))?$`)

// ParseArXiv parses the arXiv API's Atom response. Each paper version (v1, v2, ...) is its
// own item sharing a series key, so deduplication links versions of one paper.
func ParseArXiv(body []byte) ([]RawItem, error) {
	var d atomDoc
	if err := xml.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("%w: arxiv: %v", ErrUnsupportedResponse, err)
	}
	out := make([]RawItem, 0, len(d.Entries))
	for _, e := range d.Entries {
		ri := atomItem(e)
		if ri.ExternalID == "" || ri.Title == "" {
			continue
		}
		if m := arxivIDRe.FindStringSubmatch(ri.ExternalID); m != nil {
			ri.SeriesKey = "arxiv:" + m[1]
			if m[2] != "" {
				ri.VersionLabel = m[2]
			}
		}
		ri.Hints = map[string]any{"preprint": true}
		out = append(out, ri)
	}
	return out, nil
}

func rootElement(body []byte) (string, error) {
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("%w: not XML: %v", ErrUnsupportedResponse, err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name.Local, nil
		}
	}
}

// ---------------------------------------------------------------------------
// GitHub releases (REST API)
// ---------------------------------------------------------------------------

type ghRelease struct {
	ID          int64      `json:"id"`
	TagName     string     `json:"tag_name"`
	Name        string     `json:"name"`
	Body        string     `json:"body"`
	HTMLURL     string     `json:"html_url"`
	PublishedAt *time.Time `json:"published_at"`
	Draft       bool       `json:"draft"`
	Prerelease  bool       `json:"prerelease"`
	Author      struct {
		Login string `json:"login"`
	} `json:"author"`
	Reactions struct {
		TotalCount int `json:"total_count"`
	} `json:"reactions"`
}

// ParseGitHubReleases parses GET /repos/{owner}/{repo}/releases. Drafts are skipped.
func ParseGitHubReleases(body []byte, repo string) ([]RawItem, error) {
	var rels []ghRelease
	if err := json.Unmarshal(body, &rels); err != nil {
		return nil, fmt.Errorf("%w: github releases: %v", ErrUnsupportedResponse, err)
	}
	out := make([]RawItem, 0, len(rels))
	for _, r := range rels {
		if r.Draft || r.ID == 0 {
			continue
		}
		name := strings.TrimSpace(r.Name)
		if name == "" {
			name = r.TagName
		}
		title := name
		if repo != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(repoName(repo))) {
			title = repo + " " + name
		}
		ri := RawItem{
			ExternalID: "github-release:" + strconv.FormatInt(r.ID, 10), URL: r.HTMLURL, Title: title,
			Summary: cleanMarkdown(r.Body), Content: r.Body, PublishedAt: r.PublishedAt,
			VersionLabel: r.TagName, Engagement: r.Reactions.TotalCount,
			Hints: map[string]any{"prerelease": r.Prerelease, "repo": repo, "tag": r.TagName},
		}
		if r.Author.Login != "" {
			ri.Authors = []string{r.Author.Login}
		}
		out = append(out, ri)
	}
	return out, nil
}

func repoName(repo string) string {
	if i := strings.LastIndexByte(repo, '/'); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// ---------------------------------------------------------------------------
// Text helpers
// ---------------------------------------------------------------------------

var (
	tagRe       = regexp.MustCompile(`(?s)<[^>]*>`)
	scriptRe    = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	wsRe        = regexp.MustCompile(`\s+`)
	mdLinkRe    = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	mdNoiseRe   = regexp.MustCompile("[#*`>_~]+")
	timeLayouts = []string{time.RFC1123Z, time.RFC1123, time.RFC3339, time.RFC3339Nano, "Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST", "2006-01-02T15:04:05Z", "2006-01-02"}
)

// cleanText strips markup (feeds often embed escaped HTML) and collapses whitespace.
func cleanText(s string) string {
	s = html.UnescapeString(s)
	s = scriptRe.ReplaceAllString(s, " ")
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
}

func cleanMarkdown(s string) string {
	s = mdLinkRe.ReplaceAllString(s, "$1")
	s = mdNoiseRe.ReplaceAllString(s, " ")
	return cleanText(s)
}

func parseTime(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			u := t.UTC()
			return &u
		}
	}
	return nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// ApplyContentPolicy keeps only what a source's terms allow (2.2).
func ApplyContentPolicy(ri RawItem, allowed string, maxSummary int) RawItem {
	switch allowed {
	case ContentFullText:
		ri.Summary = truncateRunes(ri.Summary, maxSummary)
	case ContentSummary:
		if ri.Summary == "" {
			ri.Summary = cleanMarkdown(ri.Content)
		}
		ri.Summary = truncateRunes(ri.Summary, maxSummary)
		ri.Content = ""
	default: // METADATA_ONLY
		ri.Summary, ri.Content = "", ""
	}
	return ri
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n])
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "…"
}
