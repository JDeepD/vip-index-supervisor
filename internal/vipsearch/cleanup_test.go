package vipsearch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestGuardedCleanupUsesOnlyKnownKeysAndStopsOnGuardOrFailure(t *testing.T) {
	for _, scenario := range []string{"success", "absent warnings", "guard before first", "guard mid-sequence", "cancel mid-sequence", "cancel with nil guard error", "command failure", "explicit error", "nil guard"} {
		t.Run(scenario, func(t *testing.T) {
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(t.TempDir(), "commands")
			t.Setenv("VIP_CLEANUP_HELPER_LOG", log)
			t.Setenv("VIP_CLEANUP_HELPER_MODE", scenario)
			client := NewClient(Target{WPCommand: strconv.Quote(exe) + " -test.run=^TestCleanupHelper$ --"})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			denied := errors.New("test: worker changed")
			guard := func(context.Context) error {
				calls++
				if scenario == "guard before first" || (scenario == "guard mid-sequence" && calls == 4) {
					return denied
				}
				if scenario == "cancel mid-sequence" && calls == 2 {
					cancel()
					return ctx.Err()
				}
				if scenario == "cancel with nil guard error" && calls == 2 {
					cancel()
				}
				return nil
			}
			if scenario == "nil guard" {
				guard = nil
			}
			res := client.ClearSyncRecordGuarded(ctx, guard)
			wantCalls := 6
			switch scenario {
			case "guard before first", "nil guard":
				wantCalls = 0
			case "guard mid-sequence":
				wantCalls = 3
			case "cancel mid-sequence", "cancel with nil guard error", "command failure", "explicit error":
				wantCalls = 1
			}
			data, _ := os.ReadFile(log)
			got := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(data) == 0 {
				got = nil
			}
			want := []string{"transient delete ep_wpcli_sync", "transient delete ep_wpcli_sync --network", "option delete ep_index_meta", "site option delete ep_index_meta", "cache delete alloptions options", "cache delete ep_index_meta options"}
			if len(got) != wantCalls {
				t.Fatalf("commands=%v, expected %d", got, wantCalls)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("unexpected mutation: %q", got[i])
				}
			}
			if (scenario == "success" || scenario == "absent warnings") == res.Failed() {
				t.Fatalf("wrong failure result: %+v", res)
			}
			if strings.HasPrefix(scenario, "guard ") && !errors.Is(res.Err, denied) {
				t.Fatalf("guard failure lost: %+v", res)
			}
		})
	}
}

// A local fake CLI; this helper never invokes WordPress or VIP.
func TestCleanupHelper(t *testing.T) {
	log := os.Getenv("VIP_CLEANUP_HELPER_LOG")
	if log == "" {
		return
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	f, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(2)
	}
	if _, err = f.WriteString(strings.Join(args, " ") + "\n"); err != nil {
		os.Exit(2)
	}
	f.Close()
	mode := os.Getenv("VIP_CLEANUP_HELPER_MODE")
	if mode == "explicit error" {
		os.Stdout.WriteString("Error: denied\n")
		os.Exit(0)
	}
	if mode == "command failure" {
		os.Exit(7)
	}
	os.Stdout.WriteString("Warning: ACF using it wrong\nStack trace:\n#0 unrelated_callback()\n")
	if mode == "absent warnings" {
		os.Stdout.WriteString("Warning: Could not delete 'ep_index_meta' option. Does it exist?\n")
	} else {
		os.Stdout.WriteString("Success: Deleted.\n")
	}
	os.Exit(0)
}
