package connector

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/dcadolph/whotoask/internal/github"
)

// TestGitHubIncrementalSkipsSnapshots verifies an incremental GitHub read skips
// the whole-repo contributor and CODEOWNERS snapshots (whose weight would double
// if folded again), passes a since bound to the issues endpoint, and stops at
// pull requests older than the watermark, folding only the delta.
func TestGitHubIncrementalSkipsSnapshots(t *testing.T) {
	t.Parallel()
	resume := map[string]Position{"o/r": {Cursor: time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)}}
	var calledContributors, calledCodeowners bool
	var issuesSince string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"name":"r","full_name":"o/r","topics":["billing"]}`)
	})
	mux.HandleFunc("/repos/o/r/contributors", func(w http.ResponseWriter, _ *http.Request) {
		calledContributors = true
		io.WriteString(w, `[{"login":"oldtimer","contributions":100}]`)
	})
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, _ *http.Request) {
		// Newest-first: a recent PR after the watermark, then one before it.
		io.WriteString(w, `[{"number":2,"title":"recent fix","user":{"login":"jane"},"labels":[{"name":"retries"}],`+
			`"updated_at":"2026-06-25T10:00:00Z"},`+
			`{"number":1,"title":"ancient","user":{"login":"jane"},"labels":[{"name":"legacy"}],`+
			`"updated_at":"2026-01-01T10:00:00Z"}]`)
	})
	mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		issuesSince = r.URL.Query().Get("since")
		io.WriteString(w, `[{"number":3,"user":{"login":"dan"},"labels":[{"name":"dashboard"}],`+
			`"title":"recent issue","updated_at":"2026-06-24T08:00:00Z"}]`)
	})
	mux.HandleFunc("/repos/o/r/contents/CODEOWNERS", func(w http.ResponseWriter, _ *http.Request) {
		calledCodeowners = true
		io.WriteString(w, `{"encoding":"base64","content":""}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := github.New("ghp-test", github.WithBaseURL(srv.URL))
	recs, err := NewGitHubWithClient(client, GitHubOptions{Repos: []string{"o/r"}, Resume: resume}).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if calledContributors {
		t.Error("incremental run fetched the whole-repo contributors snapshot; it must be skipped")
	}
	if calledCodeowners {
		t.Error("incremental run fetched CODEOWNERS; it must be skipped")
	}
	if issuesSince == "" {
		t.Error("incremental run did not pass a since bound to the issues endpoint")
	}

	byID := make(map[string]Record)
	for _, r := range recs {
		byID[r.PersonID] = r
	}
	if jane := byID["github:jane"]; !slices.Contains(jane.Topics, "retries") {
		t.Errorf("recent PR author jane topics = %v, want retries", jane.Topics)
	}
	if jane := byID["github:jane"]; slices.Contains(jane.Topics, "legacy") {
		t.Error("a pre-watermark PR was folded; the newest-first break should have stopped it")
	}
	if _, ok := byID["github:oldtimer"]; ok {
		t.Error("a contributor-only login appeared though contributors was skipped")
	}
	if dan := byID["github:dan"]; !slices.Contains(dan.Topics, "dashboard") {
		t.Errorf("recent issue author dan topics = %v, want dashboard", dan.Topics)
	}
}

// githubTestRepo describes one repository the test server answers for.
type githubTestRepo struct {
	// Pulls is the JSON body of the pull request listing.
	Pulls string
	// Issues is the JSON body of the issue listing.
	Issues string
	// IssuesStatus is the status the issue listing answers with, zero for 200.
	IssuesStatus int
	// RepoStatus is the status the repository lookup answers with, zero for 200.
	RepoStatus int
}

// githubTestClient serves the given repositories, each with one contributor
// named after the repository, and counts the contributor calls per repository.
func githubTestClient(t *testing.T, repos map[string]githubTestRepo) (*github.Client, *sync.Map) {
	t.Helper()
	var contributorCalls sync.Map
	mux := http.NewServeMux()
	for full, repo := range repos {
		name := full[strings.Index(full, "/")+1:]
		mux.HandleFunc("/repos/"+full, func(w http.ResponseWriter, _ *http.Request) {
			if repo.RepoStatus != 0 {
				w.WriteHeader(repo.RepoStatus)
				return
			}
			fmt.Fprintf(w, `{"name":%q,"full_name":%q,"topics":["billing"]}`, name, full)
		})
		mux.HandleFunc("/repos/"+full+"/contributors", func(w http.ResponseWriter, _ *http.Request) {
			contributorCalls.Store(full, true)
			fmt.Fprintf(w, `[{"login":"keeper-%s","contributions":100}]`, name)
		})
		mux.HandleFunc("/repos/"+full+"/pulls", func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, cmp.Or(repo.Pulls, "[]"))
		})
		mux.HandleFunc("/repos/"+full+"/issues", func(w http.ResponseWriter, _ *http.Request) {
			if repo.IssuesStatus != 0 {
				w.WriteHeader(repo.IssuesStatus)
				return
			}
			io.WriteString(w, cmp.Or(repo.Issues, "[]"))
		})
		mux.HandleFunc("/repos/"+full+"/contents/", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return github.New("ghp-test", github.WithBaseURL(srv.URL)), &contributorCalls
}

// TestGitHubSkippedRepoKeepsItsPlace verifies a repository that fails adds
// nothing, even when its pull requests were already read, is named as skipped,
// and is left out of the positions so its saved cursor does not move. The
// repositories that were read still report where they stopped.
func TestGitHubSkippedRepoKeepsItsPlace(t *testing.T) {
	t.Parallel()
	client, _ := githubTestClient(t, map[string]githubTestRepo{
		"o/good": {
			Pulls: `[{"number":4,"title":"fix","user":{"login":"jane"},"labels":[{"name":"retries"}],` +
				`"updated_at":"2026-06-25T10:00:00Z"}]`,
		},
		"o/bad": {
			Pulls: `[{"number":9,"title":"half read","user":{"login":"mallory"},"labels":[{"name":"ledger"}],` +
				`"updated_at":"2026-06-26T10:00:00Z"}]`,
			IssuesStatus: http.StatusForbidden,
		},
	})
	src := NewGitHubWithClient(client, GitHubOptions{Repos: []string{"o/good", "o/bad"}})
	recs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	for _, r := range recs {
		if r.PersonID == "github:mallory" || r.PersonID == "github:keeper-bad" {
			t.Errorf("%s came from a repository that failed halfway; it must add nothing", r.PersonID)
		}
	}
	if diff := gocmp.Diff([]string{"o/bad"}, src.Skipped()); diff != "" {
		t.Errorf("skipped (-want +got):\n%s", diff)
	}
	positions := src.Positions()
	if _, moved := positions["o/bad"]; moved {
		t.Errorf("positions = %v, want no entry for the skipped repository", positions)
	}
	if len(positions) != 1 {
		t.Errorf("positions = %v, want the one repository that was read", positions)
	}
	want := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	if got := positions["o/good"]; !got.Cursor.Equal(want) || got.Seen["pr:4"] == "" {
		t.Errorf("o/good position = %+v, want cursor %v with pr:4 seen", got, want)
	}
	if _, ok := src.PullPeople()[PullRef{Repo: "o/bad", Number: 9}]; ok {
		t.Error("the failed repository's pull request reached PullPeople")
	}
}

// TestGitHubUnchangedBoundaryAddsNothing verifies a second read from the saved
// position, with nothing changed, counts nothing: the items at the cursor come
// back because the bound is inclusive, and each is recognized and left out. An
// item that changed since is counted.
func TestGitHubUnchangedBoundaryAddsNothing(t *testing.T) {
	t.Parallel()
	pulls := `[{"number":4,"title":"fix","user":{"login":"jane"},"labels":[{"name":"retries"}],` +
		`"updated_at":"2026-06-25T10:00:00Z"}]`
	issues := `[{"number":5,"user":{"login":"dan"},"labels":[{"name":"dashboard"}],` +
		`"title":"slow","updated_at":"2026-06-25T10:00:00Z"}]`
	client, _ := githubTestClient(t, map[string]githubTestRepo{"o/r": {Pulls: pulls, Issues: issues}})
	first := NewGitHubWithClient(client, GitHubOptions{Repos: []string{"o/r"}})
	if _, err := first.Fetch(context.Background()); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	saved := first.Positions()

	// Test 0: Nothing changed, so the second read names no one.
	second := NewGitHubWithClient(client, GitHubOptions{Repos: []string{"o/r"}, Resume: saved})
	recs, err := second.Fetch(context.Background())
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	for _, r := range recs {
		if r.Kind == KindPerson {
			t.Errorf("an unchanged second read produced %s with topics %v", r.PersonID, r.Topics)
		}
	}
	if diff := gocmp.Diff(saved, second.Positions()); diff != "" {
		t.Errorf("an unchanged read moved the position (-want +got):\n%s", diff)
	}

	// Test 1: The issue changed, so it is counted and the pull request is not.
	changed := `[{"number":5,"user":{"login":"dan"},"labels":[{"name":"dashboard"}],` +
		`"title":"slow","updated_at":"2026-06-27T09:00:00Z"}]`
	client, _ = githubTestClient(t, map[string]githubTestRepo{"o/r": {Pulls: pulls, Issues: changed}})
	third := NewGitHubWithClient(client, GitHubOptions{Repos: []string{"o/r"}, Resume: saved})
	recs, err = third.Fetch(context.Background())
	if err != nil {
		t.Fatalf("third Fetch: %v", err)
	}
	var people []string
	for _, r := range recs {
		if r.Kind == KindPerson {
			people = append(people, r.PersonID)
		}
	}
	if diff := gocmp.Diff([]string{"github:dan"}, people); diff != "" {
		t.Errorf("people after one changed issue (-want +got):\n%s", diff)
	}
}

// TestGitHubRepoWithoutPositionIsReadWhole verifies an incremental run reads a
// repository it has no position for in full, contributors included, while a
// repository with a position skips the snapshot. This is how a repository new
// to the org, or one an earlier run could not read, gets its history.
func TestGitHubRepoWithoutPositionIsReadWhole(t *testing.T) {
	t.Parallel()
	client, contributorCalls := githubTestClient(t, map[string]githubTestRepo{"o/old": {}, "o/new": {}})
	src := NewGitHubWithClient(client, GitHubOptions{
		Repos:  []string{"o/old", "o/new"},
		Resume: map[string]Position{"o/old": {Cursor: time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)}},
	})
	if _, err := src.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, called := contributorCalls.Load("o/old"); called {
		t.Error("the repository with a position had its contributors snapshot read again")
	}
	if _, called := contributorCalls.Load("o/new"); !called {
		t.Error("the repository with no position was not read whole")
	}
	if got := src.Positions()["o/new"]; got.Cursor.IsZero() {
		t.Error("a repository with no dated items finished with no cursor, so every later run would read it whole")
	}
}
