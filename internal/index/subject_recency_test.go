package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/dcadolph/whotoask/internal/connector"
)

// TestOldOwnerActiveElsewhereLosesOnRecency walks a real repository through
// the git connector into the index. Victor did heavy terraform work two years
// ago and fixed a typo in the docs yesterday; Carol did terraform work this
// month. Carol must rank first on terraform, because Victor's fresh docs
// commit says nothing about terraform, and terraform must read as work Victor
// has moved on from.
func TestOldOwnerActiveElsewhereLosesOnRecency(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	commit := func(rel, name, email string, when time.Time) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(when.String()), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := wt.Add(rel); err != nil {
			t.Fatalf("add: %v", err)
		}
		sig := &object.Signature{Name: name, Email: email, When: when}
		if _, err := wt.Commit("touch "+rel, &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	now := time.Now()
	for i, rel := range []string{"infra/vpc.tf", "infra/cluster.tf", "infra/dns.tf", "infra/iam.tf", "infra/vpc.tf"} {
		commit(rel, "Victor Old", "victor@example.com", now.AddDate(-2, 0, 3*i))
	}
	commit("infra/cluster.tf", "Carol Lee", "carol@example.com", now.AddDate(0, 0, -14))
	commit("infra/nodepool.tf", "Carol Lee", "carol@example.com", now.AddDate(0, 0, -3))
	commit("docs/readme.md", "Victor Old", "victor@example.com", now.AddDate(0, 0, -1))

	recs, err := connector.NewGitHistory(connector.GitOptions{
		Paths: []string{dir}, SinceDays: 1000,
	}).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	ix := New()
	ix.Build(recs)

	got := ix.Search("terraform", 5)
	if len(got) == 0 || got[0].Person.ID != "carol@example.com" {
		t.Errorf("terraform top = %s, want carol: victor's docs commit says nothing about terraform",
			topID(got))
	}
	victor := ix.Graph.People["victor@example.com"]
	if victor == nil {
		t.Fatal("victor missing from the index")
	}
	if victor.Topics["terraform"] == 0 {
		t.Error("victor holds no terraform weight, want his old work kept and decayed")
	}
	if victor.Recent["terraform"] != 0 {
		t.Errorf("victor recent terraform = %.3f, want none", victor.Recent["terraform"])
	}
}
