package vipsearch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestFeatureListIgnoresUnrelatedWarnings(t *testing.T) {
	for _, tc := range []struct {
		output string
		want   map[string]bool
		valid  bool
	}{
		{diagnosticNoise + "\x1b[32mActive features:\x1b[0m\nusers\nterms\nsearch\n", map[string]bool{"users": true, "terms": true}, true},
		{"Active features:\n", map[string]bool{}, true},
		{"users\nActive features:\nWarning: terms\n#0 callback(\"comments\")\n", map[string]bool{}, true},
		{"Active features:\nusers\nWarning: unrelated\ncomments\n", map[string]bool{"users": true, "comments": true}, true},
		{diagnosticNoise, nil, false},
		{"Registered features:\nusers\n", nil, false},
		{"Warning: Active features:\nusers\n", nil, false},
		{"Active features:\nusers\nActive features:\nterms\n", nil, false},
	} {
		got, err := parseFeatureList(tc.output, "Active features:")
		if (err == nil) != tc.valid || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("output=%q got=%v err=%v", tc.output, got, err)
		}
	}
}

func TestFeatureActivationIsGuardedAndVerified(t *testing.T) {
	for _, tc := range []struct {
		mode        string
		success     bool
		activations int
	}{
		{"success", true, 1},
		{"already active", true, 0},
		{"unavailable", false, 0},
		{"inconsistent", false, 0},
		{"unknown features", false, 0},
		{"failed feature read", false, 0},
		{"busy", false, 0},
		{"unknown status", false, 0},
		{"activation failed", false, 1},
		{"activation error", false, 1},
		{"no acknowledgement", false, 1},
		{"still inactive", false, 1},
		{"unknown verification", false, 1},
		{"missing indexable", false, 1},
		{"cancelled", false, 0},
		{"unsupported", false, 0},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			client, dir := featureTestClient(t, tc.mode)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.mode == "cancelled" {
				cancel()
			}
			slug := "users"
			if tc.mode == "unsupported" {
				slug = "--all"
			}
			res := client.ActivateIndexingFeature(ctx, slug)
			if res.Succeeded() != tc.success {
				t.Fatalf("unexpected activation result: %+v", res)
			}
			if client.LastResult().Succeeded() != tc.success || client.LastResult().Output != res.Output {
				t.Fatal("verification replaced the activation result")
			}
			data, _ := os.ReadFile(filepath.Join(dir, "commands"))
			activations := 0
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				switch line {
				case "activate-feature users":
					activations++
				case "", "list-features --all", "list-features", "get-indexing-status", "index-versions list user --format=json", "index-versions list user":
				default:
					t.Fatalf("unexpected command (must not index, unlock, stop, or disable): %q", line)
				}
			}
			if activations != tc.activations {
				t.Fatalf("activations=%d want=%d, commands=%s", activations, tc.activations, data)
			}
			if tc.mode == "success" && string(data) != "list-features --all\nlist-features\nget-indexing-status\nactivate-feature users\nlist-features --all\nlist-features\nindex-versions list user --format=json\n" {
				t.Fatalf("missing pre/post verification or wrong order: %s", data)
			}
		})
	}
}

func TestFeatureActivationSupportsTermsAndComments(t *testing.T) {
	for _, slug := range []string{"terms", "comments"} {
		t.Run(slug, func(t *testing.T) {
			client, dir := featureTestClient(t, "success")
			if res := client.ActivateIndexingFeature(context.Background(), slug); !res.Succeeded() {
				t.Fatalf("could not activate %s: %+v", slug, res)
			}
			data, _ := os.ReadFile(filepath.Join(dir, "commands"))
			if !strings.Contains(string(data), "activate-feature "+slug+"\n") ||
				!strings.Contains(string(data), "index-versions list "+featureIndexable(slug)+" --format=json\n") {
				t.Fatalf("wrong feature/indexable mapping: %s", data)
			}
		})
	}
}

func featureTestClient(t *testing.T, mode string) (*Client, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv("VIP_FEATURE_HELPER_DIR", dir)
	t.Setenv("VIP_FEATURE_HELPER_MODE", mode)
	// These helpers have no goroutines to drain at exit; avoid the race
	// runtime's extra one-second exit sleep per fake CLI invocation.
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	return NewClient(Target{WPCommand: strconv.Quote(exe) + " -test.run=^TestFeatureCLIHelper$ --"}), dir
}

// A local fake CLI. Only temporary files are read/written; never WordPress/VIP.
func TestFeatureCLIHelper(t *testing.T) {
	dir := os.Getenv("VIP_FEATURE_HELPER_DIR")
	if dir == "" {
		return
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) < 2 || args[0] != "vip-search" {
		os.Exit(2)
	}
	args = args[1:]
	f, err := os.OpenFile(filepath.Join(dir, "commands"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(2)
	}
	if _, err := fmt.Fprintln(f, strings.Join(args, " ")); err != nil {
		os.Exit(2)
	}
	f.Close()
	mode := os.Getenv("VIP_FEATURE_HELPER_MODE")
	active, _ := os.ReadFile(filepath.Join(dir, "active"))
	fmt.Print(diagnosticNoise)
	switch args[0] {
	case "list-features":
		if mode == "unknown features" || (mode == "unknown verification" && len(active) > 0) {
			os.Exit(0)
		}
		if mode == "failed feature read" {
			fmt.Println("Registered features:\nusers")
			os.Exit(7)
		}
		if len(args) == 2 && args[1] == "--all" {
			fmt.Println("Registered features:\nterms\ncomments\nsearch")
			if mode != "unavailable" && mode != "inconsistent" {
				fmt.Println("users")
			}
		} else {
			fmt.Println("Active features:")
			if mode == "already active" || mode == "inconsistent" {
				fmt.Println("users")
			} else if len(active) > 0 {
				fmt.Println(string(active))
			}
		}
	case "get-indexing-status":
		switch mode {
		case "busy":
			fmt.Println(`{"indexing":true}`)
		case "unknown status":
			fmt.Println(`{"indexing":null}`)
		default:
			fmt.Println(`{"indexing":false}`)
		}
	case "activate-feature":
		if len(args) != 2 || featureIndexable(args[1]) == "" {
			os.Exit(2)
		}
		if mode == "no acknowledgement" {
			os.Exit(0)
		}
		fmt.Println("Success: Feature activated")
		if mode == "activation failed" {
			os.Exit(7)
		}
		if mode == "activation error" {
			fmt.Println("Error: failed to save settings")
			os.Exit(0)
		}
		if mode != "still inactive" {
			if err := os.WriteFile(filepath.Join(dir, "active"), []byte(args[1]), 0600); err != nil {
				os.Exit(2)
			}
		}
		fmt.Println("Warning: This feature requires a re-index. You may want to run the index command next.")
	case "index-versions":
		if mode == "missing indexable" {
			fmt.Println("Error: Indexable user not found. Is the feature active?")
			os.Exit(1)
		}
		fmt.Println(`[{"number":1,"active":true,"document_count":0}]`)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}
