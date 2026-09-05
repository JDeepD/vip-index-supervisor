package vipsearch

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// IndexingFeature is one optional feature that registers an indexable offered
// by our indexing wizard. Other search features are deliberately out of scope.
type IndexingFeature struct {
	Slug       string
	Indexable  string
	Registered bool
	Active     bool
}

func featureIndexable(slug string) string {
	switch slug {
	case "users":
		return "user"
	case "terms":
		return "term"
	case "comments":
		return "comment"
	default:
		return ""
	}
}

// The upstream list-features command emits a heading followed by bare slugs,
// not JSON. Match only our known slugs after an exact heading so unrelated
// warning/stack-trace text is never interpreted as a feature name.
func parseFeatureList(output, heading string) (map[string]bool, error) {
	features := make(map[string]bool)
	found := false
	for _, line := range strings.Split(StripANSI(output), "\n") {
		line = strings.TrimSpace(line)
		if line == heading {
			if found {
				return nil, fmt.Errorf("ambiguous feature-list output")
			}
			found = true
			continue
		}
		if found && featureIndexable(line) != "" {
			features[line] = true
		}
	}
	if !found {
		return nil, fmt.Errorf("could not recognize feature-list output; expected %q", heading)
	}
	return features, nil
}

func (c *Client) IndexingFeatures(ctx context.Context) ([]IndexingFeature, error) {
	read := func(heading string, args ...string) (map[string]bool, error) {
		res := c.run(ctx, 2*time.Minute, args...)
		if res.Failed() {
			return nil, fmt.Errorf("%s: %s", strings.Join(args, " "), strings.Join(res.DescribeFailure(), "\n"))
		}
		return parseFeatureList(res.Output, heading)
	}
	registered, err := read("Registered features:", "list-features", "--all")
	if err != nil {
		return nil, err
	}
	active, err := read("Active features:", "list-features")
	if err != nil {
		return nil, err
	}
	var rows []IndexingFeature
	for _, slug := range []string{"users", "terms", "comments"} {
		if active[slug] && !registered[slug] {
			return nil, fmt.Errorf("inconsistent feature state for %s; refresh before activating", slug)
		}
		rows = append(rows, IndexingFeature{Slug: slug, Indexable: featureIndexable(slug), Registered: registered[slug], Active: active[slug]})
	}
	return rows, nil
}

// ActivateIndexingFeature is for an explicitly confirmed TUI action only.
// It rechecks availability and idle status, performs at most one activation,
// and verifies both the feature and its indexable in fresh CLI processes.
// It never starts indexing or repairs locks to make activation possible.
func (c *Client) ActivateIndexingFeature(ctx context.Context, slug string) (result RunResult) {
	defer func() { c.LastRun = result }()
	indexable := featureIndexable(slug)
	if indexable == "" {
		return RunResult{Err: fmt.Errorf("unsupported indexing feature %q", slug)}
	}
	rows, err := c.IndexingFeatures(ctx)
	if err != nil {
		return RunResult{Err: err}
	}
	var selected IndexingFeature
	for _, row := range rows {
		if row.Slug == slug {
			selected = row
		}
	}
	if !selected.Registered {
		return RunResult{Err: fmt.Errorf("feature %s is not registered on this target", slug)}
	}
	if selected.Active {
		if len(c.Versions(ctx, indexable)) == 0 {
			return RunResult{Err: fmt.Errorf("feature %s reports active, but indexable %s could not be verified", slug, indexable)}
		}
		return RunResult{Output: fmt.Sprintf("Feature %s is already active; indexable %s is registered. Nothing was changed.", slug, indexable), acknowledged: true}
	}
	st := c.Status(ctx)
	if st == nil || st.Indexing {
		return RunResult{Err: fmt.Errorf("indexing is active or its status is unknown; no feature was activated. Confirm indexing is idle, then retry")}
	}
	if ctx.Err() != nil {
		return RunResult{Err: ctx.Err()}
	}
	result = c.run(ctx, 2*time.Minute, "activate-feature", slug)
	if !result.Succeeded() {
		return result
	}
	rows, err = c.IndexingFeatures(ctx)
	if err != nil {
		result.Err = fmt.Errorf("activation acknowledged, but verification failed: %w", err)
		return result
	}
	active := false
	for _, row := range rows {
		if row.Slug == slug {
			active = row.Registered && row.Active
		}
	}
	if !active || len(c.Versions(ctx, indexable)) == 0 {
		result.Err = fmt.Errorf("activation acknowledged, but the active feature and registered indexable could not both be verified. Refresh Features before retrying")
	}
	return result
}
