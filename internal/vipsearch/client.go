package vipsearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Client wraps a Target with typed helpers for the read-only vip-search
// commands. LastRun is kept so a caller that parsed nothing can show what the
// command actually said rather than reporting a bare "none reported".
type Client struct {
	Target  Target
	LastRun RunResult
}

func NewClient(target Target) *Client { return &Client{Target: target} }

func (c *Client) LastResult() RunResult { return c.LastRun }

func (c *Client) AddVersion(ctx context.Context, indexable string) RunResult {
	return c.run(ctx, 5*time.Minute, "index-versions", "add", indexable)
}

func (c *Client) run(ctx context.Context, timeout time.Duration, args ...string) RunResult {
	res := c.Target.Run(ctx, timeout, args...)
	c.LastRun = res
	return res
}

// IndexingStatus is the payload of `get-indexing-status`.
type IndexingStatus struct {
	Indexing      bool           `json:"indexing"`
	Method        string         `json:"method"`
	TotalItems    int64          `json:"total_items"`
	ItemsIndexed  int64          `json:"items_indexed"`
	StartDateTime string         `json:"start_date_time"`
	CurrentSync   *SyncItem      `json:"current_sync_item"`
	SyncStack     []SyncItem     `json:"sync_stack"`
	Raw           map[string]any `json:"-"`
}

// SyncItem describes one queued or running sync.
type SyncItem struct {
	Indexable    string `json:"indexable"`
	Total        int64  `json:"total"`
	Synced       int64  `json:"synced"`
	Failed       int64  `json:"failed"`
	Skipped      int64  `json:"skipped"`
	LastObjectID int64  `json:"last_processed_object_id"`
}

// Status fetches and parses `get-indexing-status`. A nil result means the
// status could not be read — which callers must report as "unknown", never as
// "idle": a broken connection must not look like a healthy quiet system.
func (c *Client) Status(ctx context.Context) *IndexingStatus {
	res := c.run(ctx, 2*time.Minute, "get-indexing-status")
	if res.Failed() {
		return nil
	}
	return parseStatus(res.Output)
}

// LastIndexedPostID is global CLI progress, not scoped to an index version or
// post-type filter. It is informational only, never an automatic resume point.
func (c *Client) LastIndexedPostID(ctx context.Context) int64 {
	res := c.run(ctx, 2*time.Minute, "get-last-indexed-post-id")
	if res.Failed() {
		return 0
	}
	documents := jsonDocuments(res.Output)
	for i := len(documents) - 1; i >= 0; i-- {
		var raw map[string]json.RawMessage
		if json.Unmarshal(documents[i], &raw) == nil {
			if id := jsonInt(raw["post_id"]); id > 0 {
				return id
			}
		}
	}
	return 0
}

// IndexVersion is one row of `index-versions list`.
type IndexVersion struct {
	Number    int
	Active    bool
	Created   string
	Activated string
	Documents int64
}

// index-versions list table row: "| 1 | 1 | ... | 3088104 |"
var reVersionRow = regexp.MustCompile(`^\|\s*(\d+)\s*\|([^|]*)\|([^|]*)\|([^|]*)\|([^|]*)\|\s*$`)
var reVersionCandidate = regexp.MustCompile(`^\|\s*\d`)

// Versions lists index versions for an indexable, preferring the
// machine-readable format and falling back to the ASCII table.
func (c *Client) Versions(ctx context.Context, indexable string) []IndexVersion {
	res := c.run(ctx, 2*time.Minute, "index-versions", "list", indexable, "--format=json")
	if !res.Failed() {
		documents := jsonDocuments(res.Output)
		for i := len(documents) - 1; i >= 0; i-- {
			var raw []map[string]any
			dec := json.NewDecoder(strings.NewReader(string(documents[i])))
			dec.UseNumber()
			if dec.Decode(&raw) == nil {
				if rows := versionsFromJSON(raw); len(rows) > 0 {
					return rows
				}
			}
		}
	}
	if ctx.Err() != nil || res.NotFound || res.TimedOut {
		return nil
	}
	return c.versionsFromTable(ctx, indexable)
}

func versionsFromJSON(raw []map[string]any) []IndexVersion {
	var rows []IndexVersion
	for _, r := range raw {
		num := intValue(r["number"])
		active, ok := boolValue(r["active"])
		if num <= 0 || int64(int(num)) != num || !ok {
			return nil
		}
		docs := intValue(r["document_count"])
		rows = append(rows, IndexVersion{
			Number:    int(num),
			Active:    active,
			Created:   strings.TrimSpace(asString(r["created_time"])),
			Activated: strings.TrimSpace(asString(r["activated_time"])),
			Documents: int64(docs),
		})
	}
	if !validVersions(rows) {
		return nil
	}
	return rows
}

func (c *Client) versionsFromTable(ctx context.Context, indexable string) []IndexVersion {
	res := c.run(ctx, 2*time.Minute, "index-versions", "list", indexable)
	if res.Failed() {
		return nil
	}
	return parseVersionsTable(res.Output)
}

func parseVersionsTable(out string) []IndexVersion {
	var rows []IndexVersion
	for _, line := range strings.Split(StripANSI(out), "\n") {
		line = strings.TrimSpace(line)
		m := reVersionRow.FindStringSubmatch(line)
		if m == nil {
			// Never turn an incomplete list into a plausible partial list.
			if reVersionCandidate.MatchString(line) {
				return nil
			}
			continue
		}
		num, err := strconv.Atoi(m[1])
		docs := toInt64(strings.TrimSpace(m[5]))
		active, ok := boolValue(strings.TrimSpace(m[2]))
		if err != nil || num <= 0 || !ok {
			return nil
		}
		rows = append(rows, IndexVersion{
			Number:    num,
			Active:    active,
			Created:   strings.TrimSpace(m[3]),
			Activated: strings.TrimSpace(m[4]),
			Documents: docs,
		})
	}
	if !validVersions(rows) {
		return nil
	}
	return rows
}

func validVersions(rows []IndexVersion) bool {
	seen := make(map[int]bool, len(rows))
	active := 0
	for _, row := range rows {
		if row.Number <= 0 || seen[row.Number] {
			return false
		}
		seen[row.Number] = true
		if row.Active {
			active++
		}
	}
	return active <= 1
}

// ActiveVersion returns the active row from a version list, or nil.
func ActiveVersion(rows []IndexVersion) *IndexVersion {
	for i := range rows {
		if rows[i].Active {
			return &rows[i]
		}
	}
	return nil
}

// CountRow is one validated entity/type/version count from `health
// validate-counts`.
type CountRow struct {
	Entity  string
	Type    string
	Version int
	DB      int64
	ES      int64
	Diff    int64
}

// CountSkip is an entity the validator refused to count, with its reason.
type CountSkip struct {
	Entity  string
	Type    string
	Version int
	Reason  string
}

// CountReport is everything `health validate-counts` said.
type CountReport struct {
	Rows       []CountRow
	Skipped    []CountSkip
	ESFailures int
	Raw        string
	Failed     bool // execution failed; any parsed rows may be incomplete
}

// Aligned reports whether every counted row matched and nothing was skipped.
func (r CountReport) Aligned() bool {
	if r.Failed || len(r.Rows) == 0 || len(r.Skipped) > 0 || r.ESFailures > 0 {
		return false
	}
	for _, row := range r.Rows {
		if row.Diff != 0 {
			return false
		}
	}
	return true
}

// "✘ inconsistencies found when counting entity: post, type: page, index_version: 2 - (DB: 25, ES: 2, Diff: -23)"
var reCountRow = regexp.MustCompile(`(?i)counting entity:\s*([\w-]+),\s*type:\s*([\w/ -]+),\s*index_version:\s*(\d+)\s*-\s*\(DB:\s*(\d+),\s*ES:\s*(\d+),\s*Diff:\s*(-?\d+)\)`)

// "skipping, because there are no documents in ES when counting entity: post, ..."
var reCountSkip = regexp.MustCompile(`(?i)skipping,\s*because\s*(.*?)\s*when counting entity:\s*([\w-]+),\s*type:\s*([\w/ -]+),\s*index_version:\s*(\d+)`)

var reESFailure = regexp.MustCompile(`(?i)failure querying ES`)

// ValidateCounts runs the slow DB-vs-ES count validation.
func (c *Client) ValidateCounts(ctx context.Context, postsOnly bool) CountReport {
	sub := "validate-counts"
	if postsOnly {
		sub = "validate-posts-count"
	}
	res := c.run(ctx, 15*time.Minute, "health", sub)
	report := CountReport{Raw: res.Output, Failed: res.Failed()}

	for _, line := range strings.Split(res.Output, "\n") {
		if m := reCountSkip.FindStringSubmatch(line); m != nil {
			v, _ := strconv.Atoi(m[4])
			report.Skipped = append(report.Skipped, CountSkip{
				Entity: m[2], Type: strings.TrimSpace(m[3]), Version: v,
				Reason: strings.TrimSpace(m[1]),
			})
			continue
		}
		if m := reCountRow.FindStringSubmatch(line); m != nil {
			v, _ := strconv.Atoi(m[3])
			db, _ := strconv.ParseInt(m[4], 10, 64)
			es, _ := strconv.ParseInt(m[5], 10, 64)
			diff, _ := strconv.ParseInt(m[6], 10, 64)
			report.Rows = append(report.Rows, CountRow{
				Entity: m[1], Type: strings.TrimSpace(m[2]), Version: v,
				DB: db, ES: es, Diff: diff,
			})
		}
	}
	report.ESFailures = len(reESFailure.FindAllString(res.Output, -1))
	return report
}

// ActivateVersion makes an index version the one serving search. Irreversible
// in effect (the previous version stops serving immediately), so callers must
// confirm with the user first.
func (c *Client) ActivateVersion(ctx context.Context, indexable string, version int) RunResult {
	return c.run(ctx, 5*time.Minute,
		"index-versions", "activate", indexable, strconv.Itoa(version), "--skip-confirm")
}

// DeleteVersion permanently removes an index version and its documents.
func (c *Client) DeleteVersion(ctx context.Context, indexable string, version int) RunResult {
	return c.run(ctx, 5*time.Minute,
		"index-versions", "delete", indexable, strconv.Itoa(version), "--skip-confirm")
}

// Succeeded reports whether a mutating command actually worked: silence or an
// Error-framed line both mean it did not.
func (r RunResult) Succeeded() bool {
	return !r.Failed() && (r.acknowledged || reSuccessLine.MatchString(StripANSI(r.Output)))
}

// Failed honours both the process exit and explicit command errors. Plugin
// warnings are not errors; a success marker cannot override a nonzero exit.
func (r RunResult) Failed() bool {
	return r.Err != nil || r.NotFound || r.TimedOut || reErrorFramedLine.MatchString(StripANSI(r.Output))
}

var reErrorFramedLine = regexp.MustCompile(`(?mi)^\s*(?:Error|(?:PHP )?Fatal error):`)

// ClearIndexLock invokes ElasticPress's cleanup command. Despite the legacy
// name, delete-transient also clears sync metadata in current ElasticPress.
// Call only after checking idle, or after an explicit operator override.
func (c *Client) ClearIndexLock(ctx context.Context) RunResult {
	res := c.run(ctx, 2*time.Minute, "delete-transient")
	// This command uses WP_CLI::log, not WP_CLI::success.
	res.acknowledged = reSyncCleared.MatchString(StripANSI(res.Output))
	c.LastRun = res
	return res
}

var reSyncCleared = regexp.MustCompile(`(?mi)^\s*(?:Sync|Index) cleared\.\s*$`)

// ClearSyncRecord performs the explicit unlock action's extra option/cache
// cleanup, including legacy keys. It must never run merely because a local
// CLI died: the remote worker could still be running. Keep all failures, not
// just the final cache command's result.
func (c *Client) ClearSyncRecord(ctx context.Context) RunResult {
	// Order matters. The object-cache deletes come last and are NOT optional:
	// delete_option() looks up the database row first and returns early when
	// it is missing, before it ever calls wp_cache_delete(). So once the row
	// is gone while a stale copy survives in cache — which happens when a
	// concurrent request re-primes the autoloaded `alloptions` blob — no
	// amount of option/transient deleting can clear it, and every command
	// still reports success. ep_index_meta is autoloaded, so get_option()
	// reads it out of `alloptions` before the individual key.
	//
	// `wp site option` (a subcommand of `wp site`) is the network variant;
	// there is no `wp site-option`.
	cleanups := [][]string{
		{"transient", "delete", "ep_wpcli_sync"},
		{"transient", "delete", "ep_wpcli_sync", "--network"},
		{"option", "delete", "ep_index_meta"},
		{"site", "option", "delete", "ep_index_meta"},
		{"cache", "delete", "alloptions", "options"},
		{"cache", "delete", "ep_index_meta", "options"},
	}
	var result RunResult
	for _, args := range cleanups {
		res := c.Target.RunWP(ctx, 2*time.Minute, args...)
		result.Output += fmt.Sprintf("%s\n%s\n", strings.Join(args, " "), res.Output)
		result.NotFound = result.NotFound || res.NotFound
		result.TimedOut = result.TimedOut || res.TimedOut
		if res.Err != nil {
			result.Err = errors.Join(result.Err, res.Err)
		}
		if ctx.Err() != nil {
			break
		}
	}
	c.LastRun = result
	return result
}

// StopIndexing asks a running index to stop.
func (c *Client) StopIndexing(ctx context.Context) RunResult {
	return c.run(ctx, 2*time.Minute, "stop-indexing")
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
