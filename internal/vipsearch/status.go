package vipsearch

import (
	"encoding/json"
	"strconv"
	"strings"
)

func intValue(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		return toInt64(string(n))
	case string:
		return toInt64(strings.TrimSpace(n))
	case float64:
		// Kept for callers of versionsFromJSON using encoding/json defaults.
		return toInt64(strconv.FormatFloat(n, 'f', -1, 64))
	default:
		return NoValue
	}
}

func jsonInt(raw json.RawMessage) int64 {
	var value any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if dec.Decode(&value) != nil {
		return NoValue
	}
	return intValue(value)
}

// Unlike Truthy, this rejects unknown representations. Unknown must not be
// interpreted as either an idle platform or an inactive, safe-to-delete index.
func boolValue(v any) (bool, bool) {
	switch b := v.(type) {
	case bool:
		return b, true
	case json.Number:
		return boolValue(string(b))
	case float64:
		if b == 0 || b == 1 {
			return b == 1, true
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(b)) {
		case "true", "1", "yes":
			return true, true
		case "false", "0", "no", "":
			return false, true
		}
	}
	return false, false
}

func parseStatus(out string) *IndexingStatus {
	documents := jsonDocuments(out)
	for i := len(documents) - 1; i >= 0; i-- {
		var raw map[string]any
		dec := json.NewDecoder(strings.NewReader(string(documents[i])))
		dec.UseNumber()
		if dec.Decode(&raw) != nil {
			continue
		}
		indexing, ok := boolValue(raw["indexing"])
		// PHP's status API emits an actual boolean; tolerate 0/1 strings too,
		// but an empty/missing field is not evidence of idle.
		if !ok || raw["indexing"] == "" {
			continue
		}
		st := &IndexingStatus{
			Indexing: indexing, Method: asString(raw["method"]),
			TotalItems: intValue(raw["total_items"]), ItemsIndexed: intValue(raw["items_indexed"]),
			StartDateTime: asString(raw["start_date_time"]), Raw: raw,
		}
		if cur, ok := raw["current_sync_item"].(map[string]any); ok {
			st.CurrentSync = parseSync(cur)
		}
		if stack, ok := raw["sync_stack"].([]any); ok {
			for _, item := range stack {
				if row, ok := item.(map[string]any); ok {
					st.SyncStack = append(st.SyncStack, *parseSync(row))
				}
			}
		}
		return st
	}
	return nil
}

func parseSync(raw map[string]any) *SyncItem {
	return &SyncItem{
		Indexable: asString(raw["indexable"]), Total: intValue(raw["total"]),
		Synced: intValue(raw["synced"]), Failed: intValue(raw["failed"]),
		Skipped: intValue(raw["skipped"]), LastObjectID: intValue(raw["last_processed_object_id"]),
	}
}
