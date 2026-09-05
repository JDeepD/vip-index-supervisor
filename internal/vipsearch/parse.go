package vipsearch

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

// The indexer prints progress in two shapes:
//
//	"Processed posts 364500 - 365000 of 3423561. Last Object ID: 3258809"
//	"Processed 500/37674. Last Object ID: 37550"
var (
	reProgressRange = regexp.MustCompile(`(?i)^\s*Processed\s+\w*\s*(\d+)\s*-\s*(\d+)\s+of\s+(\d+)\.?(?:\s+Last Object ID:\s*(\d+))?\s*$`)
	reProgressSlash = regexp.MustCompile(`(?i)^\s*Processed\s+(\d+)\s*/\s*(\d+)\.?(?:\s+Last Object ID:\s*(\d+))?\s*$`)
	reLastObjectID  = regexp.MustCompile(`(?i)^\s*Last Object ID:\s*(\d+)\s*$`)
	reIndexedCount  = regexp.MustCompile(`(?i)^\s*Number of \w+ indexed:\s*(\d+)\s*$`)
	reMemoryUsage   = regexp.MustCompile(`(?i)^\s*Memory Usage:\s*(\d+(?:\.\d+)?)\s*mb(?:\s+\(Peak:\s*\d+(?:\.\d+)?\s*mb\))?\s*$`)
	reANSI          = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\[[0-?]*[ -/]*[@-~]`)
	reLockError     = regexp.MustCompile(`(?i)^\s*Error:\s*An index is already occurring\b`)
	reIndexSuccess  = regexp.MustCompile(`(?i)^\s*Success:\s*Done!\s*$`)
	reSuccessLine   = regexp.MustCompile(`(?mi)^\s*Success:\s*\S`)
)

// StripANSI removes colour/cursor escape codes. VIP-CLI colourises output
// even when piped, so "Success: Done!" really arrives as
// "\x1b[32mSuccess:\x1b[39m Done!" — every marker and regex in this package
// assumes the caller has stripped that, or matches would silently fail on
// the exact lines that matter most.
func StripANSI(s string) string { return reANSI.ReplaceAllString(s, "") }

func IsLockError(line string) bool    { return reLockError.MatchString(StripANSI(line)) }
func IsIndexSuccess(line string) bool { return reIndexSuccess.MatchString(StripANSI(line)) }

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
	MemoryUsage  string // current indexer usage in MB; empty when absent, never the peak
}

// NoValue marks an absent field in Progress.
const NoValue = -1

// ParseProgress extracts progress numbers from a single output line.
func ParseProgress(line string) Progress {
	line = StripANSI(line)
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
	if m := reMemoryUsage.FindStringSubmatch(line); m != nil {
		if _, err := strconv.ParseFloat(m[1], 64); err == nil {
			p.MemoryUsage = m[1] + " MB"
		}
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
// PHP/plugin warnings (including their file names and stack traces) are not
// evidence of a failed command. Only explicit error lines are classified.
var reErrorFraming = regexp.MustCompile(`(?i)^\s*(?:Error|(?:PHP )?Fatal error):`)

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

// jsonDocuments tolerates warning/trace prefixes and suffixes. A successfully
// decoded outer document is skipped in full; its nested objects are never
// mistaken for another command result. Callers validate the expected schema.
func jsonDocuments(out string) []json.RawMessage {
	out = StripANSI(out)
	var documents []json.RawMessage
	for pos := 0; pos < len(out); {
		next := strings.IndexAny(out[pos:], "[{")
		if next < 0 {
			break
		}
		pos += next
		dec := json.NewDecoder(strings.NewReader(out[pos:]))
		var raw json.RawMessage
		if dec.Decode(&raw) == nil {
			documents = append(documents, raw)
			pos += int(dec.InputOffset())
		} else {
			pos++
		}
	}
	return documents
}

// ExtractJSON returns the last compatible outer JSON document. Decode into a
// fresh value so failed candidates cannot leave partially populated fields.
func ExtractJSON(out string, into any) bool {
	dst := reflect.ValueOf(into)
	if dst.Kind() != reflect.Pointer || dst.IsNil() {
		return false
	}
	documents := jsonDocuments(out)
	for i := len(documents) - 1; i >= 0; i-- {
		value := reflect.New(dst.Elem().Type())
		if json.Unmarshal(documents[i], value.Interface()) == nil {
			dst.Elem().Set(value.Elem())
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
