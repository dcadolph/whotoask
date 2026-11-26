package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// savedDemo writes the simulated company to a fresh data directory, the way
// demo --save-index does, and returns the directory.
func savedDemo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "demo")
	if _, stderr, err := runCmd(t, "demo", "--save-index", dir); err != nil {
		t.Fatalf("demo --save-index: %v\n%s", err, stderr)
	}
	return dir
}

// TestDemoSaveIndex verifies demo --save-index writes an index and episodes
// that the rest of the commands read as a real data directory.
func TestDemoSaveIndex(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "demo")
	_, stderr, err := runCmd(t, "demo", "--save-index", dir)
	if err != nil {
		t.Fatalf("demo --save-index: %v\n%s", err, stderr)
	}
	for _, name := range []string{"index.json", "episodes.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("demo --save-index wrote no %s: %v", name, err)
		}
	}
	if !strings.Contains(string(stderr), "wrote the sample company to "+dir) {
		t.Errorf("stderr does not name the directory:\n%s", stderr)
	}
	out, _, err := runCmd(t, "status", "--data-dir", dir, "--json")
	if err != nil {
		t.Fatalf("status on the saved demo: %v", err)
	}
	if !strings.Contains(string(out), "people") {
		t.Errorf("status on the saved demo names no people: %s", out)
	}
}

// TestDemoHelpCountsItsSources checks the demo help names exactly the sources
// the saved simulation was built from, so the count it states is the count the
// status command reports.
func TestDemoHelpCountsItsSources(t *testing.T) {
	t.Parallel()
	dir := savedDemo(t)
	out, _, err := runCmd(t, "status", "--data-dir", dir, "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var st struct {
		Sources map[string]int `json:"sources"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		t.Fatalf("decode status: %v\n%s", err, out)
	}
	help, _, err := runCmd(t, "demo", "--help")
	if err != nil {
		t.Fatalf("demo --help: %v", err)
	}
	// Each indexed source is named in the help by its display name.
	shown := map[string]string{
		"org-csv": "org chart CSV", "slack": "Slack", "git": "git", "github": "GitHub",
		"jira": "Jira", "confluence": "Confluence", "pagerduty": "PagerDuty",
		"codeowners": "CODEOWNERS",
	}
	if len(st.Sources) != len(shown) {
		t.Errorf("simulation indexed %d sources %v, help names %d", len(st.Sources), st.Sources, len(shown))
	}
	for name := range st.Sources {
		label, ok := shown[name]
		if !ok || !strings.Contains(string(help), label) {
			t.Errorf("help does not name the simulated source %q", name)
		}
	}
	if strings.Contains(string(help), "all eight") {
		t.Errorf("help still claims the simulation covers all sources:\n%s", help)
	}
	if !strings.Contains(string(help), "eight sources") {
		t.Errorf("help does not state the count:\n%s", help)
	}
}

// TestDemoSavedLinksAreStable checks a saved demo stores no link to the local
// server the simulation was built against.
func TestDemoSavedLinksAreStable(t *testing.T) {
	t.Parallel()
	dir := savedDemo(t)
	raw, err := os.ReadFile(filepath.Join(dir, "episodes.json"))
	if err != nil {
		t.Fatalf("read episodes: %v", err)
	}
	if strings.Contains(string(raw), "127.0.0.1") {
		t.Error("saved demo episodes link to the local build server")
	}
	if !strings.Contains(string(raw), "https://jira.example.com/browse/") {
		t.Error("saved demo episodes carry no stable Jira link")
	}
}
