package vipsearch

import (
	"strings"
	"testing"
)

func TestParseCurrentMemoryUsage(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"Memory Usage: 171.99mb (Peak: 173.43mb)", "171.99 MB"},
		{"Memory Usage: 180.29mb (Peak: 181.77mb)", "180.29 MB"},
		{"Memory Usage: 90mb", "90 MB"},
		{"Memory Usage: 0.00mb (Peak: 12mb)", "0.00 MB"},
		{"\x1b[32mMemory Usage:\x1b[0m 176.08mb (Peak: 177.58mb)\r", "176.08 MB"},
		{"  memory usage: 185.22 MB (peak: 186.73 MB)  ", "185.22 MB"},
		{"Warning: Memory Usage: 171.99mb (Peak: 173.43mb)", ""},
		{`#0 callback("Memory Usage: 171.99mb")`, ""},
		{"Peak: 173.43mb", ""},
		{"Memory Usage: -1mb", ""},
		{"Memory Usage: NaNmb", ""},
		{"Memory Usage: Infmb", ""},
		{"Memory Usage: 171.99", ""},
		{"Memory Usage: 171.99mb trailing warning", ""},
		{"Memory Usage: 171.99mb (Peak: broken)", ""},
		{"Memory Usage: " + strings.Repeat("9", 400) + "mb", ""},
		{"Processed 300/1000. Last Object ID: 700", ""},
	} {
		t.Run(tc.line, func(t *testing.T) {
			p := ParseProgress(tc.line)
			if p.MemoryUsage != tc.want {
				t.Fatalf("memory=%q want=%q", p.MemoryUsage, tc.want)
			}
			if tc.want != "" && (p.Done != NoValue || p.Total != NoValue || p.LastObjectID != NoValue || p.IndexedCount != NoValue) {
				t.Fatalf("memory line became indexing progress: %+v", p)
			}
		})
	}
}
