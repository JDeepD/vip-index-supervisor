package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

// This file renders the read-only commands (status, info, versions, counts,
// health) into plain strings the output screen can display and scroll.

func renderStatus(st *vipsearch.IndexingStatus) string {
	var b strings.Builder
	b.WriteString(styleHeading.Render("Indexing status") + "\n")
	if st.Indexing {
		b.WriteString("  state          " + styleOK.Render("● running") + "\n")
	} else {
		b.WriteString("  state          " + styleDim.Render("○ idle") + "\n")
	}

	cur := st.CurrentSync
	if cur == nil {
		cur = &vipsearch.SyncItem{}
	}
	indexable := cur.Indexable
	if indexable == "" {
		indexable = "—"
	}
	b.WriteString("  indexable      " + styleAccent.Render(indexable) + "\n")

	total := firstPositive(cur.Total, st.TotalItems)
	synced := firstNonNegative(cur.Synced, st.ItemsIndexed)
	if total > 0 {
		frac := float64(synced) / float64(total)
		b.WriteString("  progress       " + progressBar(frac, 30) + fmt.Sprintf(" %5.1f%%\n", frac*100))
		b.WriteString(fmt.Sprintf("  objects        %s / %s\n", groupInt(synced), groupInt(total)))
	} else if st.Indexing {
		b.WriteString(fmt.Sprintf("  objects        %s / (total not yet determined)\n", groupInt(synced)))
	}
	if cur.LastObjectID > 0 {
		b.WriteString("  last object id " + groupInt(cur.LastObjectID) + "\n")
	}
	failed := fmt.Sprintf("  failed/skipped %d / %d\n", cur.Failed, cur.Skipped)
	if cur.Failed > 0 {
		failed = styleErr.Render(failed)
	}
	b.WriteString(failed)
	if st.StartDateTime != "" {
		b.WriteString("  started        " + st.StartDateTime + "\n")
	}
	if len(st.SyncStack) > 0 {
		var queued []string
		for _, item := range st.SyncStack {
			queued = append(queued, item.Indexable)
		}
		b.WriteString("  queued next    " + styleWarn.Render(strings.Join(queued, ", ")) + "\n")
	}
	return b.String()
}

func renderStatusUnavailable(client *vipsearch.Client) string {
	var b strings.Builder
	b.WriteString(styleErr.Render("Could not read indexing status.") + "\n")
	for _, line := range client.LastRun.DescribeFailure() {
		b.WriteString(styleDim.Render("  "+line) + "\n")
	}
	return b.String()
}

func renderVersions(client *vipsearch.Client, ctx context.Context, indexables []string) string {
	var b strings.Builder
	for _, indexable := range indexables {
		b.WriteString(styleHeading.Render("Index versions — "+indexable) + "\n")
		rows := client.Versions(ctx, indexable)
		if len(rows) == 0 {
			// `index-versions list` always reports at least version 1, so an
			// empty result means the command failed — show its own words.
			b.WriteString(styleWarn.Render("  (none parsed)") + "\n")
			for _, line := range client.LastRun.DescribeFailure() {
				b.WriteString(styleDim.Render("    "+line) + "\n")
			}
			continue
		}
		b.WriteString(styleDim.Render(fmt.Sprintf("  %-8s %-10s %14s", "version", "state", "documents")) + "\n")
		for _, v := range rows {
			state := styleDim.Render("—")
			if v.Active {
				state = styleOK.Render("● active")
			}
			docs := groupInt(v.Documents)
			if v.Active && v.Documents == 0 {
				docs = styleErr.Render(docs + "  ⚠ EMPTY")
			}
			b.WriteString("  " + padRight(strconv.Itoa(v.Number), 9) + padRight(state, 10) + padLeft(docs, 15) + "\n")
		}
	}
	return b.String()
}

func renderCounts(report vipsearch.CountReport) string {
	var b strings.Builder
	b.WriteString(styleHeading.Render("DB vs ES document counts") + "\n")

	if report.Failed {
		b.WriteString(styleErr.Render("  the validate-counts command could not run") + "\n")
		return b.String()
	}
	if report.ESFailures > 0 {
		b.WriteString(styleWarn.Render(fmt.Sprintf("  ⚠ %d 'failure querying ES' warning(s)", report.ESFailures)) + "\n")
		b.WriteString(styleDim.Render("  expected while a bulk index runs — counts are unreliable until indexing is idle") + "\n")
	}
	if len(report.Rows) > 0 {
		b.WriteString(styleDim.Render(fmt.Sprintf("  %-8s %-14s %-4s %12s %12s %10s",
			"entity", "type", "ver", "DB", "ES", "diff")) + "\n")
		for _, r := range report.Rows {
			diff := styleOK.Render(fmt.Sprintf("%10s", "0"))
			if r.Diff != 0 {
				diff = styleErr.Render(fmt.Sprintf("%+10d", r.Diff))
			}
			b.WriteString(fmt.Sprintf("  %-8s %-14s %-4d %12s %12s %s\n",
				r.Entity, r.Type, r.Version, groupInt(r.DB), groupInt(r.ES), diff))
		}
	}
	for _, s := range report.Skipped {
		b.WriteString(styleErr.Render(fmt.Sprintf("  ✗ %s/%s (v%d) — %s", s.Entity, s.Type, s.Version, s.Reason)) + "\n")
	}
	if len(report.Rows) == 0 && len(report.Skipped) == 0 {
		b.WriteString(styleWarn.Render("  no count rows parsed") + "\n")
		for _, line := range lastLines(report.Raw, 6) {
			b.WriteString(styleDim.Render("    "+line) + "\n")
		}
	}
	if report.Aligned() {
		b.WriteString("\n" + styleOK.Render("  ✓ every counted row matches") + "\n")
	}
	return b.String()
}

func renderHealth(ctx context.Context, client *vipsearch.Client) string {
	var b strings.Builder
	b.WriteString(styleHeading.Render("Health check") + "\n")

	st := client.Status(ctx)
	if st == nil {
		// Reporting "idle" here would be a lie: we do not know. That false
		// negative is what makes a broken connection look like a healthy
		// quiet system.
		b.WriteString(styleErr.Render("  indexing: unknown — could not read status") + "\n")
		for _, line := range client.LastRun.DescribeFailure() {
			b.WriteString(styleDim.Render("    "+line) + "\n")
		}
		return b.String()
	}
	if st.Indexing {
		b.WriteString("  indexing      " + styleWarn.Render("running") + styleDim.Render("  (counts unreliable while it runs)") + "\n")
	} else {
		b.WriteString("  indexing      idle\n")
	}

	versions := client.Versions(ctx, "post")
	if active := vipsearch.ActiveVersion(versions); active != nil {
		line := fmt.Sprintf("  active index  v%d — %s documents", active.Number, groupInt(active.Documents))
		if active.Documents == 0 {
			b.WriteString(styleErr.Render(line) + "\n")
			b.WriteString(styleErr.Render("  ⚠ active index is EMPTY — search will return nothing") + "\n")
		} else {
			b.WriteString(styleOK.Render(line) + "\n")
		}
	} else if len(versions) == 0 {
		b.WriteString(styleErr.Render("  active index  could not read the version list") + "\n")
	} else {
		b.WriteString(styleWarn.Render("  active index  none of the reported versions is active") + "\n")
	}

	b.WriteString("\n" + renderCounts(client.ValidateCounts(ctx, false)))
	return b.String()
}

func renderInfo(ctx context.Context, client *vipsearch.Client) string {
	var b strings.Builder
	b.WriteString(styleHeading.Render("Environment") + "\n")
	b.WriteString("  " + client.Target.Label() + "\n")
	if !client.Target.IsVIP() {
		b.WriteString(styleDim.Render("  via "+strings.Join(client.Target.Base(), " ")+" (direct, not VIP-CLI)") + "\n")
	}
	b.WriteString("\n" + renderVersions(client, ctx, []string{"post"}) + "\n")

	if st := client.Status(ctx); st != nil {
		b.WriteString(renderStatus(st))
	} else {
		b.WriteString(renderStatusUnavailable(client))
	}

	b.WriteString("\n" + styleHeading.Render("Resume point") + "\n")
	if last := client.LastIndexedPostID(ctx); last > 0 {
		b.WriteString("  last indexed post id  " + styleAccent.Render(groupInt(last)) + "\n")
	} else {
		b.WriteString(styleDim.Render("  (none reported)") + "\n")
	}
	return b.String()
}

// -- small shared helpers -----------------------------------------------------

// padRight and padLeft align table cells by *display* width. fmt's %Ns pads
// by bytes, which drifts as soon as a cell carries ANSI colour codes or
// multi-byte runes (●, —); lipgloss.Width sees through both.
func padRight(cell string, width int) string {
	if gap := width - lipgloss.Width(cell); gap > 0 {
		return cell + strings.Repeat(" ", gap)
	}
	return cell
}

func padLeft(cell string, width int) string {
	if gap := width - lipgloss.Width(cell); gap > 0 {
		return strings.Repeat(" ", gap) + cell
	}
	return cell
}

func progressBar(frac float64, width int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac * float64(width))
	return styleAccent.Render(strings.Repeat("█", filled)) +
		styleDim.Render(strings.Repeat("░", width-filled))
}

func groupInt(n int64) string {
	if n < 0 {
		return "?"
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, digit := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(digit)
	}
	return b.String()
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Hour {
		return fmt.Sprintf("%02d:%02d", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
}

// firstPositive mirrors the platform quirk where totals arrive as -1 while
// still being determined; rendering that raw would show a negative bar.
func firstPositive(values ...int64) int64 {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

func firstNonNegative(values ...int64) int64 {
	for _, v := range values {
		if v >= 0 {
			return v
		}
	}
	return 0
}

func lastLines(text string, n int) []string {
	var lines []string
	for _, ln := range strings.Split(strings.TrimSpace(text), "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			lines = append(lines, s)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
