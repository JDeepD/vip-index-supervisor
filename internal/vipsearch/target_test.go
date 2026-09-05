package vipsearch

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func TestCommandArguments(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", []string{"wp"}},
		{`wp --path="/srv/my site" --url='https://example.test'`, []string{"wp", "--path=/srv/my site", "--url=https://example.test"}},
		{`"/a b/wp" --flag=''`, []string{"/a b/wp", "--flag="}},
		{`"C:\Program Files\wp.exe"`, []string{`C:\Program Files\wp.exe`}},
		{`C:\tools\wp.exe`, []string{`C:\tools\wp.exe`}},
	} {
		if got := (Target{WPCommand: tc.in}).BaseWP(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: %#v", tc.in, got)
		}
	}
	if err := (Target{WPCommand: `wp --path="oops`}).Validate(); err == nil {
		t.Fatal("bad quoting accepted")
	}
}

func TestClientWithNoisyCLI(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("VIP_SUPERVISOR_TEST_CLI", "1")
	target := Target{WPCommand: strconv.Quote(exe) + " -test.run=^TestCLIHelper$ --"}
	t.Setenv("VIP_SUPERVISOR_TEST_OUTPUT", diagnosticNoise+`{"indexing":true,"items_indexed":600}`)
	client := NewClient(target)
	if st := client.Status(context.Background()); st == nil || !st.Indexing || st.ItemsIndexed != 600 {
		t.Fatalf("%+v; %+v", st, client.LastRun)
	}
	for _, output := range []string{"Sync cleared.", "Index cleared."} {
		t.Setenv("VIP_SUPERVISOR_TEST_OUTPUT", diagnosticNoise+output+"\n")
		if res := client.ClearIndexLock(context.Background()); !res.Succeeded() {
			t.Fatalf("upstream plain-text acknowledgement rejected: %+v", res)
		}
	}
	t.Setenv("VIP_SUPERVISOR_TEST_OUTPUT", diagnosticNoise+`{"indexing":false}`)
	t.Setenv("VIP_SUPERVISOR_TEST_EXIT", "1")
	if st := client.Status(context.Background()); st != nil {
		t.Fatalf("parsed status from a failed CLI: %+v", st)
	}
	t.Setenv("VIP_SUPERVISOR_TEST_EXIT", "0")
	t.Setenv("VIP_SUPERVISOR_TEST_OUTPUT", diagnosticNoise+`{"post_id":"1944405","time":"2026-09-05 03:02:15"}`+"\n[trace]")
	if id := client.LastIndexedPostID(context.Background()); id != 1944405 {
		t.Fatalf("ID=%d", id)
	}
	t.Setenv("VIP_SUPERVISOR_TEST_OUTPUT", diagnosticNoise+`[{"number":"1","active":"1","document_count":"5000"}]`)
	if rows := client.Versions(context.Background(), "post"); len(rows) != 1 || rows[0].Documents != 5000 {
		t.Fatalf("%+v", rows)
	}
	t.Setenv("VIP_SUPERVISOR_TEST_OUTPUT", diagnosticNoise+"Success: Deleted.\n")
	t.Setenv("VIP_SUPERVISOR_FAIL_CLEANUP", "1")
	if res := client.ClearSyncRecord(context.Background()); res.Succeeded() || res.Err == nil {
		t.Fatalf("earlier cleanup failure lost: %+v", res)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if res := target.Run(ctx, time.Second, "anything"); res.Err == nil {
		t.Fatal("cancelled command succeeded")
	}
}

// Runs only as a subprocess; never invokes wp or VIP.
func TestCLIHelper(t *testing.T) {
	if os.Getenv("VIP_SUPERVISOR_TEST_CLI") != "1" {
		return
	}
	fmt.Print(os.Getenv("VIP_SUPERVISOR_TEST_OUTPUT"))
	for _, arg := range os.Args {
		if arg == "transient" && os.Getenv("VIP_SUPERVISOR_FAIL_CLEANUP") == "1" {
			os.Exit(1)
		}
	}
	code, _ := strconv.Atoi(os.Getenv("VIP_SUPERVISOR_TEST_EXIT"))
	os.Exit(code)
}
