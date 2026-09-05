package vipsearch

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const diagnosticNoise = "Warning: acf was called incorrectly in /plugins/acf.php on line 403\n" +
	"Stack trace:\n#0 /plugins/acf.php(401): callback([broken])\n" +
	"#1 {main}\nDebug: {\"plugin\":\"acf\",\"context\":[1,2]}\n"

func TestStatusWithDiagnostics(t *testing.T) {
	out := diagnosticNoise + `{"indexing":true,"method":"cli","total_items":"9000","items_indexed":600,"current_sync_item":{"indexable":"post","total":9000,"synced":"600","last_processed_object_id":"8401"},"sync_stack":[]}` +
		"\nWarning: closing [trace]\n{\"plugin\":\"acf\"}"
	st := parseStatus(out)
	if st == nil || !st.Indexing || st.ItemsIndexed != 600 || st.TotalItems != 9000 || st.CurrentSync == nil || st.CurrentSync.LastObjectID != 8401 {
		t.Fatalf("incorrect status: %+v", st)
	}
}

func TestUnknownStatusIsNotIdle(t *testing.T) {
	for _, out := range []string{"", diagnosticNoise, "{}", "[]", `{"message":"ok"}`, `{"indexing":null}`, `{"indexing":"maybe"}`, `{"indexing":2}`, `{"indexing":[]}`, `{"indexing":""}`, `{"indexing":false`} {
		t.Run(out, func(t *testing.T) {
			if st := parseStatus(out); st != nil {
				t.Fatalf("unknown became %+v", st)
			}
		})
	}
	for _, out := range []string{`{"indexing":false}`, `{"indexing":"0","current_sync_item":false,"sync_stack":false}`, `{"indexing":0,"current_sync_item":[],"sync_stack":[]}`} {
		if st := parseStatus(out); st == nil || st.Indexing {
			t.Fatalf("valid idle rejected: %s", out)
		}
	}
}

func TestExtractJSONOuterDocumentAndNoPartialMutation(t *testing.T) {
	var got struct {
		Items  []int `json:"items"`
		Number int   `json:"number"`
	}
	out := diagnosticNoise + `{"items":[1,2],"number":7}` + "\nstack [warning]\n" + `{"number":"invalid","items":[9]}`
	if !ExtractJSON(out, &got) || got.Number != 7 || !reflect.DeepEqual(got.Items, []int{1, 2}) {
		t.Fatalf("got %+v", got)
	}
	var rows []int
	if ExtractJSON(`{"nested":[1,2]}`, &rows) {
		t.Fatal("nested object mistaken for result")
	}
	if ExtractJSON(out, nil) {
		t.Fatal("nil accepted")
	}
}

func TestWarningAndTraceMarkers(t *testing.T) {
	for _, line := range []string{
		"Warning: ACF on line 403: unauthorized",
		"PHP Warning: invalid token in plugin.php on line 401",
		`#0 handler("Success: Done!")`,
		"Notice: an index is already occurring",
		`#1 callback("Processed 300/1000. Last Object ID: 999")`,
	} {
		if ClassifyFatal(line) != "" || IsIndexSuccess(line) || IsLockError(line) || ParseProgress(line).LastObjectID != NoValue {
			t.Errorf("diagnostic treated as command signal: %s", line)
		}
	}
	if ClassifyFatal("\x1b[31mError:\x1b[0m Unauthorized (401)") != "authorization" {
		t.Fatal("auth failure missed")
	}
	if !IsLockError("  Error: An index is already occurring. Try again later.") {
		t.Fatal("lock missed")
	}
	if !IsIndexSuccess("\x1b[32mSuccess:\x1b[39m Done!\r") {
		t.Fatal("success missed")
	}
}

func TestParseProgress(t *testing.T) {
	for _, tc := range []struct {
		line              string
		done, total, last int64
	}{
		{"Processed posts 300 - 600 of 9000. Last Object ID: 7001", 600, 9000, 7001},
		{"Processed 600/9000. Last Object ID: 7001", 600, 9000, 7001},
		{"\x1b[0KProcessed 600/9000.\r", 600, 9000, -1},
		{"Last Object ID: 7001", -1, -1, 7001},
		{"Processed 999999999999999999999999/9000.", -1, 9000, -1},
	} {
		p := ParseProgress(tc.line)
		if p.Done != tc.done || p.Total != tc.total || p.LastObjectID != tc.last {
			t.Errorf("%s: %+v", tc.line, p)
		}
	}
}

func TestMutationSuccessRequiresExitAndAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result RunResult
		want   bool
	}{
		{"noise then success", RunResult{Output: diagnosticNoise + "Success: Activated version 2\n"}, true},
		{"warnings only", RunResult{Output: diagnosticNoise}, false},
		{"empty", RunResult{}, false},
		{"nonzero after success", RunResult{Output: "Success: Done!", Err: errors.New("exit 1")}, false},
		{"error after success", RunResult{Output: "Success: Done!\n\x1b[31mError:\x1b[0m failed"}, false},
		{"fatal", RunResult{Output: "Success: Done!\nPHP Fatal error: bad call"}, false},
		{"quoted marker", RunResult{Output: `#0 call("Success: Done!")`}, false},
		{"timeout", RunResult{Output: "Success: Done!", TimedOut: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.result.Succeeded(); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestVersionNumbersAndUnknownCounts(t *testing.T) {
	rows := versionsFromJSON([]map[string]any{
		{"number": "2", "active": "0", "document_count": "1000"},
		{"number": json.Number("1"), "active": true},
	})
	if len(rows) != 2 || rows[0].Number != 2 || rows[0].Active || rows[0].Documents != 1000 || !rows[1].Active || rows[1].Documents != -1 {
		t.Fatalf("%+v", rows)
	}
	for _, raw := range []map[string]any{
		{"number": 1.5, "active": false}, {"number": "bad", "active": false}, {"number": 1.0, "active": "maybe"},
	} {
		if rows := versionsFromJSON([]map[string]any{raw}); len(rows) != 0 {
			t.Fatalf("unsafe version accepted: %+v", rows)
		}
	}
}

func TestVersionTablePreservesUnknownActiveCount(t *testing.T) {
	for _, count := range []string{"", "N/A", "false", "-1", "99999999999999999999999999"} {
		out := diagnosticNoise + "| number | active | created_time | activated_time | document_count |\n" +
			"| 1 | 1 | yesterday | yesterday | " + count + " |\n| 2 | 0 | today | | 100 |\n"
		rows := parseVersionsTable(out)
		if len(rows) != 2 || !rows[0].Active || rows[0].Documents != NoValue || rows[1].Documents != 100 {
			t.Fatalf("active row lost or count misrepresented (%q): %+v", count, rows)
		}
	}
}

func TestInvalidVersionListsAreRejected(t *testing.T) {
	for _, out := range []string{
		"| 2 | 0 | today | | 100 |\n| 1 | 1 | truncated",
		"| 1 | 1 | yesterday | | 100 |\n| 1 | 0 | today | | 100 |",
		"| 1 | 1 | yesterday | | 100 |\n| 2 | 1 | today | | 100 |",
		"| 999999999999999999999999 | 1 | yesterday | | 100 |",
	} {
		if rows := parseVersionsTable(out); len(rows) > 0 {
			t.Fatalf("unsafe partial table accepted: %+v", rows)
		}
	}
	for _, raw := range [][]map[string]any{
		{{"number": "1", "active": true}, {"number": "1", "active": false}},
		{{"number": "1", "active": true}, {"number": "2", "active": true}},
	} {
		if rows := versionsFromJSON(raw); len(rows) > 0 {
			t.Fatalf("ambiguous JSON versions accepted: %+v", rows)
		}
	}
}

func TestCountReportNeedsEvidence(t *testing.T) {
	for _, r := range []CountReport{{}, {Failed: true, Rows: []CountRow{{}}}, {Skipped: []CountSkip{{}}}, {ESFailures: 1}, {Rows: []CountRow{{Diff: 1}}}} {
		if r.Aligned() {
			t.Fatalf("false healthy result: %+v", r)
		}
	}
	if !(CountReport{Rows: []CountRow{{Diff: 0}}}).Aligned() {
		t.Fatal("matching counts rejected")
	}
	if !reCountRow.MatchString("counting entity: post, type: breaking-news, index_version: 2 - (DB: 25, ES: 25, Diff: 0)") {
		t.Fatal("hyphenated type ignored")
	}
}

func FuzzParsers(f *testing.F) {
	for _, s := range []string{diagnosticNoise, `{"indexing":true}`, "Processed 300/9000. Last Object ID: 8999", "\x1b[32mSuccess: Done!"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 8192 {
			t.Skip()
		}
		parseStatus(s)
		ParseProgress(s)
		ClassifyFatal(s)
		var value any
		ExtractJSON(s, &value)
		clean := StripANSI(s)
		if strings.Contains(clean, "\x1b[32m") {
			t.Fatal("colour sequence survived")
		}
	})
}
