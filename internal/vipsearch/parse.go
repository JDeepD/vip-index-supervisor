package vipsearch

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// The indexer prints progress in two shapes:
//
//	"Processed posts 364500 - 365000 of 3423561. Last Object ID: 3258809"
//	"Processed 500/37674. Last Object ID: 37550"
var (
	reProgressRange = regexp.MustCompile(`(?i)Processed\s+\w*\s*(\d+)\s*-\s*(\d+)\s+of\s+(\d+)\.?(?:\s+Last Object ID:\s*(\d+))?`)
	reProgressSlash = regexp.MustCompile(`(?i)Processed\s+(\d+)\s*/\s*(\d+)\.?(?:\s+Last Object ID:\s*(\d+))?`)
	reLastObjectID  = regexp.MustCompile(`(?i)Last Object ID:\s*(\d+)`)
	reIndexedCount  = regexp.MustCompile(`(?i)Number of \w+ indexed:\s*(\d+)`)
	reANSI          = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
)

// StripANSI removes colour/cursor escape codes. VIP-CLI colourises output
// even when piped, so "Success: Done!" really arrives as
// "\x1b[32mSuccess:\x1b[39m Done!" — every marker and regex in this package
// assumes the caller has stripped that, or matches would silently fail on
// the exact lines that matter most.
func StripANSI(s string) string { return reANSI.ReplaceAllString(s, "") }

const (
	// SuccessMarker is the line the indexer prints on clean completion.
	SuccessMarker = "Success: Done!"
	// LockErrorMarker means a previous run died holding the index transient.
	LockErrorMarker = "an index is already occurring"
)

// Progress is whatever structured signal one output line carried.
// Nil-able ints are modelled as -1 = absent, which keeps callers honest about
// checking presence without pointer noise.
type Progress struct {
	Done         int64
	Total        int64
	LastObjectID int64
	IndexedCount int64
}

// NoValue marks an absent field in Progress.
const NoValue = -1

// ParseProgress extracts progress numbers from a single output line.
func ParseProgress(line string) Progress {
	p := Progress{Done: NoValue, Total: NoValue, LastObjectID: NoValue, IndexedCount: NoValue}

	if m := reProgressRange.FindStringSubmatch(line); m != nil {
		p.Done = toInt64(m[2])
		p.Total = toInt64(m[3])
		if m[4] != "" {
			p.LastObjectID = toInt64(m[4])
		}
	} else if m := reProgressSlash.FindStringSubmatch(line); m != nil {
		p.Done = toInt64(m[1])
		p.Total = toInt64(m[2])
		if m[3] != "" {
			p.LastObjectID = toInt64(m[3])
		}
	}

	if p.LastObjectID == NoValue {
		if m := reLastObjectID.FindStringSubmatch(line); m != nil {
			p.LastObjectID = toInt64(m[1])
		}
	}
	if m := reIndexedCount.FindStringSubmatch(line); m != nil {
		p.IndexedCount = toInt64(m[1])
	}
	return p
}

// Failures that will never succeed on retry. Without this, a typo'd post type
// or an expired token burns the whole backoff budget and buries the real
// message under "no progress" warnings.
var fatalPatterns = []struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`(?i)Parameter errors`), "invalid parameters"},
	{regexp.MustCompile(`(?i)Unknown\s+--\S+\s+parameter`), "invalid parameters"},
	{regexp.MustCompile(`(?i)is not a registered (?:post type|indexable|taxonomy)`), "unknown target"},
	{regexp.MustCompile(`(?i)\bno such (?:index|version)\b`), "target does not exist"},
	{regexp.MustCompile(`(?i)\b(?:401|403)\b|unauthorized|not authorized|access denied|forbidden`), "authorization"},
	{regexp.MustCompile(`(?i)you (?:are not|must be) logged in|authentication failed|invalid token|token (?:has )?expired`), "authentication"},
	{regexp.MustCompile(`(?i)(?:environment|app|site) (?:was not found|does not exist|not found)`), "unknown environment"},
}

// A line must BE an error before any fatal pattern is considered — not merely
// contain error-ish words. WP-CLI and VIP-CLI both prefix real failures at the
// start of the line, so anchor there. Matching anywhere would let a post
// titled "Unauthorized biography of a forbidden city", echoed back by
// --show-errors, abort the whole run as an auth failure.
var reErrorFraming = regexp.MustCompile(`(?i)^\s*(?:Error|Fatal error|Warning)\b`)

// ClassifyFatal returns a reason if this output line means retrying is
// pointless, or "" when the line is not a fatal error.
func ClassifyFatal(line string) string {
	line = StripANSI(line)
	if !reErrorFraming.MatchString(line) {
		return ""
	}
	for _, p := range fatalPatterns {
		if p.re.MatchString(line) {
			return p.reason
		}
	}
	return ""
}

// ExtractJSON pulls the outermost JSON document out of noisy VIP-CLI output.
//
// VIP-CLI prefixes its own banner lines, so the payload is rarely the whole
// of stdout. Candidates are tried outermost-first: `get-indexing-status`
// returns an object that *contains* a `sync_stack` array, and matching that
// inner array instead would silently yield the wrong document.
func ExtractJSON(out string, into any) bool {
	type candidate struct {
		start int
		blob  string
	}
	var candidates []candidate
	for _, pair := range [][2]string{{"[", "]"}, {"{", "}"}} {
		start := strings.Index(out, pair[0])
		end := strings.LastIndex(out, pair[1])
		if start != -1 && end > start {
			candidates = append(candidates, candidate{start, out[start : end+1]})
		}
	}
	if len(candidates) == 2 && candidates[1].start < candidates[0].start {
		candidates[0], candidates[1] = candidates[1], candidates[0]
	}
	for _, c := range candidates {
		if json.Unmarshal([]byte(c.blob), into) == nil {
			return true
		}
	}
	return false
}

// Truthy interprets WP-CLI's inconsistent boolean rendering: the `active`
// column arrives as true, 1, "1", "" or even the string "0" depending on
// plugin version and output format.
func Truthy(v any) bool {
	switch val := v.(type) {
	case nil:
		return false
	case bool:
		return val
	case float64:
		return val != 0
	case string:
		s := strings.ToLower(strings.TrimSpace(val))
		return s != "" && s != "0" && s != "false" && s != "no" && s != "null" && s != "none"
	default:
		return true
	}
}

func toInt64(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return NoValue
	}
	return n
}
