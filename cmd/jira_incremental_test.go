package cmd

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dcadolph/whotoask/internal/connector"
	"github.com/dcadolph/whotoask/internal/fakeapi"
	"github.com/dcadolph/whotoask/internal/state"
)

// TestJiraIncrementalReindex runs a full index then a --merge re-index against a
// fake Jira and verifies the wiring end to end: the first run records a
// watermark, the second run sends a since-bounded query and folds rather than
// replaces, and both people survive the partial re-read.
func TestJiraIncrementalReindex(t *testing.T) {
	issues := []fakeapi.JiraIssue{
		{Key: "KAFKA-1", Summary: "consumer group rebalance", Description: "session timeout",
			AssigneeEmail: "amaki@apache.invalid", ProjectKey: "KAFKA", ProjectName: "Kafka",
			Updated: "2026-06-20T09:30:00.000+0000"},
		{Key: "KAFKA-2", Summary: "dashboard down", Description: "grafana panels",
			AssigneeEmail: "brao@apache.invalid", ProjectKey: "KAFKA", ProjectName: "Kafka",
			Updated: "2026-06-19T09:30:00.000+0000"},
	}
	fake := &fakeapi.Jira{Issues: issues, ServerMode: true}
	srv := fake.Server()
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	// First run: a full index writes the watermark.
	if _, stderr, err := runCmd(t, "index", "--source", "jira", "--jira-server",
		"--jira-url", srv.URL, "--jira-project", "KAFKA", "--data-dir", dir); err != nil {
		t.Fatalf("full index: %v\n%s", err, stderr)
	}
	st, err := state.Load(filepath.Join(dir, "index.state.json"), nil)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	wm, ok := st.Get("jira", "project:KAFKA")
	if !ok || wm.Units[""].Cursor.IsZero() {
		t.Fatalf("no jira position after a full index: %+v", st.Watermarks)
	}

	// Second run: --merge is incremental, so it queries updated-since, oldest first.
	before := len(fake.Queries)
	if _, stderr, err := runCmd(t, "index", "--source", "jira", "--merge", "--jira-server",
		"--jira-url", srv.URL, "--jira-project", "KAFKA", "--data-dir", dir); err != nil {
		t.Fatalf("incremental index: %v\n%s", err, stderr)
	}
	var sawIncremental bool
	for _, raw := range fake.Queries[before:] {
		dec, _ := url.QueryUnescape(raw)
		if strings.Contains(dec, "updated >=") && strings.Contains(dec, "ORDER BY updated ASC") {
			sawIncremental = true
		}
	}
	if !sawIncremental {
		t.Errorf("incremental run did not send a since-bounded query, got: %v", fake.Queries[before:])
	}

	// Both people survive the incremental re-index rather than being dropped.
	out, _, err := runCmd(t, "ask", "--data-dir", dir, "kafka", "--limit", "5")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	for _, want := range []string{"amaki", "brao"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("person %q missing after incremental re-index:\n%s", want, out)
		}
	}
}

// heldJiraTopics returns the topics each person's held Jira record carries, read
// from the sources file beside the index. Each person is keyed by "amaki" or
// "brao", whichever their record names, since a Server site may give a person
// an id, a name, or an email.
func heldJiraTopics(t *testing.T, dir string) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "index.sources.json"))
	if err != nil {
		t.Fatalf("read sources: %v", err)
	}
	var side struct {
		Sources map[string][]connector.Record `json:"sources"`
	}
	if err := json.Unmarshal(raw, &side); err != nil {
		t.Fatalf("decode sources: %v", err)
	}
	out := make(map[string][]string)
	for _, rec := range side.Sources["jira"] {
		if rec.Kind != connector.KindPerson {
			continue
		}
		for _, who := range []string{"amaki", "brao"} {
			if strings.Contains(rec.PersonID+" "+rec.Email+" "+rec.Name, who) {
				out[who] = append(append([]string(nil), rec.Topics...), rec.WeakTopics...)
			}
		}
	}
	return out
}

// TestJiraRefreshWithNoChangeAddsNothing verifies a refresh that finds nothing
// new leaves the held records exactly as they were. The query reaches a little
// below the cursor, so the newest issues come back every run. Before, each run
// counted them again: after one full read and four merges a person's topics
// had grown fivefold, and whoever touched the latest ticket gained its weight
// four times a day. An issue that really changed is still counted.
func TestJiraRefreshWithNoChangeAddsNothing(t *testing.T) {
	issues := []fakeapi.JiraIssue{
		{Key: "KAFKA-1", Summary: "consumer group rebalance", Description: "session timeout",
			AssigneeEmail: "amaki@apache.invalid", ProjectKey: "KAFKA", ProjectName: "Kafka",
			Labels: []string{"billing"}, Updated: "2026-06-20T09:30:00.000+0000"},
		{Key: "KAFKA-2", Summary: "dashboard down", Description: "grafana panels",
			AssigneeEmail: "brao@apache.invalid", ProjectKey: "KAFKA", ProjectName: "Kafka",
			Labels: []string{"payments"}, Updated: "2026-06-20T09:29:00.000+0000"},
	}
	fake := &fakeapi.Jira{Issues: issues, ServerMode: true}
	srv := fake.Server()
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	args := []string{"index", "--source", "jira", "--jira-server", "--jira-url", srv.URL,
		"--jira-project", "KAFKA", "--data-dir", dir}
	if _, stderr, err := runCmd(t, args...); err != nil {
		t.Fatalf("full index: %v\n%s", err, stderr)
	}
	want := heldJiraTopics(t, dir)
	if len(want["amaki"]) == 0 || len(want["brao"]) == 0 {
		t.Fatalf("the full read held no topics: %v", want)
	}

	// Test 0: Four refreshes with nothing changed.
	merge := append(append([]string{}, args...), "--merge")
	for run := range 4 {
		if _, stderr, err := runCmd(t, merge...); err != nil {
			t.Fatalf("merge %d: %v\n%s", run, err, stderr)
		}
	}
	if diff := cmp.Diff(want, heldJiraTopics(t, dir)); diff != "" {
		t.Errorf("held topics changed across refreshes that found nothing new (-want +got):\n%s", diff)
	}

	// Test 1: One issue changes, so it is counted once more and the other is not.
	fake.Issues[0].Updated = "2026-06-21T10:00:00.000+0000"
	if _, stderr, err := runCmd(t, merge...); err != nil {
		t.Fatalf("merge after a change: %v\n%s", err, stderr)
	}
	got := heldJiraTopics(t, dir)
	if len(got["amaki"]) != 2*len(want["amaki"]) {
		t.Errorf("amaki topics = %v, want the changed issue counted once more over %v",
			got["amaki"], want["amaki"])
	}
	if diff := cmp.Diff(want["brao"], got["brao"]); diff != "" {
		t.Errorf("brao's unchanged issue was counted again (-want +got):\n%s", diff)
	}
}

// TestJiraExplicitJQLNotIncremental verifies an explicit --jira-jql source is
// never treated as incremental. The connector ignores the watermark for a raw
// query, so folding it would stack a copy every run; instead the run replaces,
// and no watermark (which would embed the raw JQL in the plain state file) is
// written.
func TestJiraExplicitJQLNotIncremental(t *testing.T) {
	issues := []fakeapi.JiraIssue{
		{Key: "KAFKA-1", Summary: "consumer rebalance", Description: "session timeout",
			AssigneeEmail: "amaki@apache.invalid", ProjectKey: "KAFKA", ProjectName: "Kafka",
			Updated: "2026-06-20T09:30:00.000+0000"},
	}
	fake := &fakeapi.Jira{Issues: issues, ServerMode: true}
	srv := fake.Server()
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	base := []string{"index", "--source", "jira", "--jira-server", "--jira-url", srv.URL,
		"--jira-jql", "project = KAFKA ORDER BY updated DESC", "--data-dir", dir}
	if _, stderr, err := runCmd(t, base...); err != nil {
		t.Fatalf("first index: %v\n%s", err, stderr)
	}
	st, err := state.Load(filepath.Join(dir, "index.state.json"), nil)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if len(st.Watermarks) != 0 {
		t.Errorf("explicit JQL wrote a watermark (raw query would leak to plain state): %+v", st.Watermarks)
	}

	before := len(fake.Queries)
	merge := append(append([]string{}, base...), "--merge")
	if _, stderr, err := runCmd(t, merge...); err != nil {
		t.Fatalf("merge index: %v\n%s", err, stderr)
	}
	for _, raw := range fake.Queries[before:] {
		dec, _ := url.QueryUnescape(raw)
		if strings.Contains(dec, "updated >=") {
			t.Errorf("explicit-JQL merge injected a since clause, should replace not fold: %s", dec)
		}
	}
	out, _, err := runCmd(t, "ask", "--data-dir", dir, "rebalance", "--limit", "3")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if !strings.Contains(string(out), "amaki") {
		t.Errorf("amaki missing after explicit-JQL merge:\n%s", out)
	}
}
