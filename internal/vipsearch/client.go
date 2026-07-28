package vipsearch

import (
	"context"
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
	var st IndexingStatus
	if !ExtractJSON(res.Output, &st) {
		return nil
	}
	return &st
}

var rePostID = regexp.MustCompile(`"post_id"\s*:\s*(\d+)`)

// LastIndexedPostID is the live resume point the platform itself records
// (post indexable only). 0 means none reported.
func (c *Client) LastIndexedPostID(ctx context.Context) int64 {
	res := c.run(ctx, 2*time.Minute, "get-last-indexed-post-id")
	if m := rePostID.FindStringSubmatch(res.Output); m != nil {
		if id, err := strconv.ParseInt(m[1], 10, 64); err == nil && id > 0 {
			return id
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
var reVersionRow = regexp.MustCompile(`^\|\s*(\d+)\s*\|([^|]*)\|([^|]*)\|([^|]*)\|\s*(\d+)?\s*\|`)

// Versions lists index versions for an indexable, preferring the
// machine-readable format and falling back to the ASCII table.
func (c *Client) Versions(ctx context.Context, indexable string) []IndexVersion {
	res := c.run(ctx, 2*time.Minute, "index-versions", "list", indexable, "--format=json")
	var raw []map[string]any
	if ExtractJSON(res.Output, &raw) {
		if rows := versionsFromJSON(raw); len(rows) > 0 {
			return rows
		}
	}
	return c.versionsFromTable(ctx, indexable)
}

func versionsFromJSON(raw []map[string]any) []IndexVersion {
	var rows []IndexVersion
	for _, r := range raw {
		num, ok := r["number"].(float64)
		if !ok {
			continue
		}
		docs, _ := r["document_count"].(float64)
		rows = append(rows, IndexVersion{
			Number:    int(num),
			Active:    Truthy(r["active"]),
			Created:   strings.TrimSpace(asString(r["created_time"])),
			Activated: strings.TrimSpace(asString(r["activated_time"])),
			Documents: int64(docs),
		})
	}
	return rows
}

func (c *Client) versionsFromTable(ctx context.Context, indexable string) []IndexVersion {
	res := c.run(ctx, 2*time.Minute, "index-versions", "list", indexable)
	var rows []IndexVersion
	for _, line := range strings.Split(res.Output, "\n") {
		m := reVersionRow.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		num, _ := strconv.Atoi(m[1])
		var docs int64
		if m[5] != "" {
			docs, _ = strconv.ParseInt(m[5], 10, 64)
		}
		rows = append(rows, IndexVersion{
			Number:    num,
			Active:    Truthy(strings.TrimSpace(m[2])),
			Created:   strings.TrimSpace(m[3]),
			Activated: strings.TrimSpace(m[4]),
			Documents: docs,
		})
	}
	return rows
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
	Failed     bool // the command itself could not run
}

// Aligned reports whether every counted row matched and nothing was skipped.
func (r CountReport) Aligned() bool {
	if len(r.Skipped) > 0 || r.ESFailures > 0 {
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
var reCountRow = regexp.MustCompile(`(?i)counting entity:\s*(\w+),\s*type:\s*([\w/ ]+),\s*index_version:\s*(\d+)\s*-\s*\(DB:\s*(\d+),\s*ES:\s*(\d+),\s*Diff:\s*(-?\d+)\)`)

// "skipping, because there are no documents in ES when counting entity: post, ..."
var reCountSkip = regexp.MustCompile(`(?i)skipping,\s*because\s*(.*?)\s*when counting entity:\s*(\w+),\s*type:\s*([\w/ ]+),\s*index_version:\s*(\d+)`)

var reESFailure = regexp.MustCompile(`(?i)failure querying ES`)

// ValidateCounts runs the slow DB-vs-ES count validation.
func (c *Client) ValidateCounts(ctx context.Context, postsOnly bool) CountReport {
	sub := "validate-counts"
	if postsOnly {
		sub = "validate-posts-count"
	}
	res := c.run(ctx, 15*time.Minute, "health", sub)
	report := CountReport{Raw: res.Output, Failed: res.NotFound || res.TimedOut}

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
	if r.NotFound || r.TimedOut || strings.TrimSpace(r.Output) == "" {
		return false
	}
	return !reErrorFramedLine.MatchString(r.Output)
}

var reErrorFramedLine = regexp.MustCompile(`(?m)^\s*Error:`)

// ClearIndexLock clears the stale "an index is already occurring" transient.
// This is only the LOCK — a killed run also leaves a sync-state record that
// keeps get-indexing-status reporting "indexing": see ClearSync.
func (c *Client) ClearIndexLock(ctx context.Context) RunResult {
	return c.run(ctx, 2*time.Minute, "delete-transient")
}

// ClearSyncRecord removes everything a killed indexing process leaves behind.
//
// A dead sync blocks all later runs from FOUR keys, not one: the ep_wpcli_sync
// transient (the flag that raises "An index is already occurring") and the
// ep_index_meta option (the progress record get-indexing-status prints), each
// in both the regular and the network/site variant. Clearing only some of them
// looks like it worked while the platform keeps reporting a sync in flight —
// which version gets read depends on whether ElasticPress is network-active.
//
// ElasticPress documents this cleanup as a single `wp eval` one-liner
// (10up/ElasticPress#1533), but wp eval is disallowed on VIP, so each key is
// deleted through its own permitted command. Deleting a key that does not
// exist is harmless.
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
	var last RunResult
	for _, args := range cleanups {
		last = c.Target.RunWP(ctx, 2*time.Minute, args...)
	}
	c.LastRun = last
	return last
}

// StopIndexing asks a running index to stop.
func (c *Client) StopIndexing(ctx context.Context) RunResult {
	return c.run(ctx, 2*time.Minute, "stop-indexing")
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
