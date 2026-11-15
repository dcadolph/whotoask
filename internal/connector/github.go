package connector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"sync"

	"github.com/dcadolph/whotoask/internal/episode"
	"github.com/dcadolph/whotoask/internal/github"

	"github.com/dcadolph/whotoask/internal/util"
)

// maxTopicWeight is where a topic's count stops counting one for one, so a
// heavy contributor outranks a one-off without a single topic dominating the
// score. Past it the count still grows, logarithmically, because a hard stop
// here makes somebody who touched an area four times indistinguishable from
// its owner who touched it three hundred, and telling those two apart is the
// entire question whotoask is asked. maxTopicRepeat is where it finally stops,
// so one long-running subject cannot crowd out everything else a person knows.
const (
	maxTopicWeight = 4
	maxTopicRepeat = 16
)

// noiseWords are common pull request and issue title words that carry no topic.
var noiseWords = map[string]bool{
	"fix": true, "fixes": true, "fixed": true, "add": true, "adds": true,
	"added": true, "update": true, "updates": true, "updated": true,
	"remove": true, "removes": true, "bump": true, "the": true, "and": true,
	"for": true, "with": true, "from": true, "into": true, "that": true,
	"this": true, "use": true, "uses": true, "new": true, "support": true,
	"make": true, "when": true, "not": true, "via": true, "run": true,
	"set": true, "get": true, "all": true, "out": true, "try": true,
}

// GitHubOptions configures the GitHub connector.
type GitHubOptions struct {
	// Repos is a list of "owner/name" repositories to index.
	Repos []string
	// Org, when set, adds the org's repositories.
	Org string
	// MaxRepos caps repositories taken from the org; zero means all returned.
	MaxRepos int
	// ResolveEmails fetches each user's profile to join by email.
	ResolveEmails bool
	// Episodes records merged changes, so recall can point back at the change
	// that fixed something.
	Episodes bool
	// Resume holds where an earlier read stopped in each repository, keyed by
	// the lowercase "owner/name", for an incremental re-index. A repository in
	// it is read from its cursor: only pull requests and issues updated at or
	// after it, without the whole-repo contributor and CODEOWNERS snapshots,
	// whose weight would double if folded again. A repository missing from it
	// is read whole, which is how a repository new to the org, or one an
	// earlier run could not read, gets its history. Nil reads every repository
	// whole.
	Resume map[string]Position
	// MaxPages raises how many pages each listing may walk. Zero keeps the
	// client default, which bounds a scheduled run; an assessment reads a
	// repository once and asks for depth deliberately.
	MaxPages int
	// BaseURL is the API root, for a GitHub Enterprise Server deployment.
	// Empty uses github.com.
	BaseURL string
	// Log receives progress lines; nil discards them.
	Log io.Writer
}

// withDefaults fills the log writer when unset.
func (o GitHubOptions) withDefaults() GitHubOptions {
	if o.Log == nil {
		o.Log = io.Discard
	}
	return o
}

// GitHub is a Source that ingests repositories. It weights people by what they
// actually work on: pull request and issue labels and titles for authors,
// reviewers, and assignees, plus repository topics for contributors and
// CODEOWNERS for path ownership.
type GitHub struct {
	// client calls the GitHub API.
	client *github.Client
	// opts holds the resolved options.
	opts GitHubOptions
	// episodes holds the merged changes seen by the last Fetch.
	episodes []episode.Episode
	// pullPeople records, per pull request, everyone who took part in it:
	// author, reviewers, assignees, and whoever actually reviewed or
	// commented. Held so review credit can be placed against the directories
	// the pull request changed, which the git history already knows.
	pullPeople map[PullRef][]string
	// positions holds where the last Fetch stopped in each repository it read.
	positions map[string]Position
	// skipped names the repositories the last Fetch could not read.
	skipped []string
}

// Positions returns where the most recent Fetch stopped in each repository it
// read to the end, keyed by the lowercase "owner/name".
func (g *GitHub) Positions() map[string]Position { return clonePositions(g.positions) }

// Skipped names the repositories the most recent Fetch could not read.
func (g *GitHub) Skipped() []string { return slices.Clone(g.skipped) }

// repoUnit returns the key a repository's position is saved under.
func repoUnit(full string) string { return strings.ToLower(full) }

// repoTally is what reading one repository adds to the people it names. Each
// repository fills its own, and the parent takes it only once the repository
// was read to the end. A repository that failed halfway therefore adds
// nothing, and reading it again from its saved position counts nothing twice.
type repoTally struct {
	// counts maps a login to how often each token was seen for it.
	counts map[string]map[string]int
	// latest maps a login to its most recent activity.
	latest map[string]time.Time
	// curated holds the tokens some repository, pull request, or issue stated
	// as a topic or label. Everything else mined from a name, title, or
	// description stays weak.
	curated map[string]bool
	// pulls records, per pull request, everyone who took part in it.
	pulls map[PullRef][]string
}

// newRepoTally returns an empty tally.
func newRepoTally() *repoTally {
	return &repoTally{
		counts:  make(map[string]map[string]int),
		latest:  make(map[string]time.Time),
		curated: make(map[string]bool),
		pulls:   make(map[PullRef][]string),
	}
}

// markCurated records tokens as stated topics.
func (t *repoTally) markCurated(tokens []string) {
	for _, tok := range tokens {
		if tok = strings.ToLower(strings.TrimSpace(tok)); tok != "" {
			t.curated[tok] = true
		}
	}
}

// bump credits login with tokens for activity at the given time. Automation
// accounts are left out.
func (t *repoTally) bump(login string, tokens []string, at time.Time) {
	if login == "" || len(tokens) == 0 || strings.HasSuffix(login, "[bot]") {
		return
	}
	c := t.counts[login]
	if c == nil {
		c = make(map[string]int)
		t.counts[login] = c
	}
	for _, tok := range tokens {
		if tok = strings.ToLower(strings.TrimSpace(tok)); tok != "" {
			c[tok]++
		}
	}
	if at.After(t.latest[login]) {
		t.latest[login] = at
	}
}

// absorb adds everything other holds into t.
func (t *repoTally) absorb(other *repoTally) {
	for login, tokens := range other.counts {
		c := t.counts[login]
		if c == nil {
			c = make(map[string]int)
			t.counts[login] = c
		}
		for tok, n := range tokens {
			c[tok] += n
		}
	}
	for login, at := range other.latest {
		if at.After(t.latest[login]) {
			t.latest[login] = at
		}
	}
	maps.Copy(t.curated, other.curated)
	maps.Copy(t.pulls, other.pulls)
}

// PullPeople returns, per pull request, the logins of everyone who took part.
// Keys carry the repository as well as the number, so two repositories'
// identically numbered pull requests never collide; the git history's
// PullDirs uses the same keys. Joined, the two say who reviewed which places
// rather than which words.
func (g *GitHub) PullPeople() map[PullRef][]string { return g.pullPeople }

// NewGitHub returns a GitHub connector authenticating with token.
func NewGitHub(token string, opts GitHubOptions) *GitHub {
	return &GitHub{
		client:     github.New(token, github.WithMaxPages(opts.MaxPages), github.WithBaseURL(opts.BaseURL)),
		opts:       opts.withDefaults(),
		pullPeople: make(map[PullRef][]string),
	}
}

// NewGitHubWithClient returns a GitHub connector using a preconfigured client.
// Tests use it to inject a client pointed at a mock server.
func NewGitHubWithClient(client *github.Client, opts GitHubOptions) *GitHub {
	if client == nil {
		panic("connector: NewGitHubWithClient requires a non-nil client")
	}
	return &GitHub{client: client, opts: opts.withDefaults(),
		pullPeople: make(map[PullRef][]string)}
}

// Ping verifies the token with a cheap authenticated request, so a wizard can
// confirm credentials before committing to a full index.
func (g *GitHub) Ping(ctx context.Context) error {
	return g.client.Ping(ctx)
}

// Fetch reads each repository and returns person records weighted by topic.
func (g *GitHub) Fetch(ctx context.Context) ([]Record, error) {
	g.episodes = nil
	// A fresh map, not a merge: participants kept from an earlier Fetch would
	// collect review credit for pull requests this fetch never saw.
	g.pullPeople = make(map[PullRef][]string)
	g.positions = make(map[string]Position)
	g.skipped = nil
	repos, err := g.repoList(ctx)
	if err != nil {
		return nil, err
	}

	// Repos are fetched concurrently, so serialize every write to the shared
	// tallies and to the log writer, which a test may back with a plain buffer.
	g.opts.Log = &lockedWriter{w: g.opts.Log}
	var mu sync.Mutex
	total := newRepoTally()

	// Per-repo results land in slots indexed by the repo's position, so the
	// combined output follows the input order however the workers finish;
	// byte-for-byte reproducibility is a product claim.
	type repoResult struct {
		recs []Record
		eps  []episode.Episode
	}
	var wg sync.WaitGroup
	// Each repository is read by its own worker, so each fills its own tally of
	// what is worked on together and the parent folds them in.
	ties := newTogether()
	sem := make(chan struct{}, repoWorkers)
	results := make([]repoResult, len(repos))
	for i, full := range repos {
		owner, name, ok := splitRepo(full)
		if !ok {
			fmt.Fprintf(g.opts.Log,
				"github: %q is not owner/name, so it was not indexed; fix the --github-repo value\n", full)
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			// Cancelled while waiting for a slot: do not launch a worker, which
			// would receive a semaphore token that was never sent and hang the
			// WaitGroup. Skip the rest so wg.Wait returns.
			continue
		}
		wg.Add(1)
		go func(i int, full, owner, name string) {
			defer wg.Done()
			defer func() { <-sem }()
			local := newTogether()
			tally := newRepoTally()
			recs, eps, pos, err := g.indexRepo(ctx, full, owner, name, tally, local)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				// Nothing this repository added is kept and its position does
				// not move, so the next run reads it from where it really
				// stopped.
				g.skipped = append(g.skipped, full)
				fmt.Fprintf(g.opts.Log, "github: skipping %s: %v\n", full, err)
				return
			}
			results[i] = repoResult{recs: recs, eps: eps}
			ties.absorb(local)
			total.absorb(tally)
			g.positions[repoUnit(full)] = pos
		}(i, full, owner, name)
	}
	wg.Wait()
	slices.Sort(g.skipped)
	g.pullPeople = total.pulls
	if len(g.skipped) > 0 {
		fmt.Fprintf(g.opts.Log,
			"github: %d repositories could not be read this time; the next refresh picks each one up "+
				"where it stopped\n", len(g.skipped))
	}
	var codeOwnerRecords []Record
	for _, r := range results {
		codeOwnerRecords = append(codeOwnerRecords, r.recs...)
		g.episodes = append(g.episodes, r.eps...)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	accounts := g.resolveAccounts(ctx, total.counts)

	records := make([]Record, 0, len(total.counts)+len(codeOwnerRecords))
	for _, login := range slices.Sorted(maps.Keys(total.counts)) {
		tokenCounts := total.counts[login]
		rec := githubPersonRecord(login, nil, accounts[login])
		rec.Topics, rec.WeakTopics = splitCurated(expandTopics(tokenCounts), total.curated)
		rec.Time = total.latest[login]
		records = append(records, rec)
	}
	records = append(records, codeOwnerRecords...)
	records = append(records, ties.records("github")...)
	return records, nil
}

// indexRepo tallies one repository's contributors, pull requests, issues, and
// CODEOWNERS into tally, returning that repo's CODEOWNERS records, its episodes,
// and the position the next read of it resumes from. A truncated listing is
// indexed as a partial set with a warning. A hard error is returned so the
// caller can skip this repo, or abort on context cancellation, without
// discarding the repos already indexed.
func (g *GitHub) indexRepo(
	ctx context.Context, full, owner, name string, tally *repoTally, ties *togetherIndex,
) ([]Record, []episode.Episode, Position, error) {
	// A repository with no saved cursor is read whole even inside an
	// incremental run: nothing is held for it, so folding its full read is
	// right, and it is the only way its history ever arrives.
	prev := g.opts.Resume[repoUnit(full)]
	incremental := !prev.Cursor.IsZero()
	seen := newBoundary(prev, time.Now().UTC(), 0)
	repo, err := g.client.Repo(ctx, owner, name)
	if err != nil {
		return nil, nil, Position{}, fmt.Errorf("repo: %w", err)
	}
	// Contributors is a whole-repo snapshot with no since filter, so re-folding
	// it on an incremental run would add each repo topic to every contributor
	// again. Skip it when reading incrementally; those people are preserved from
	// the last full run, and a periodic --full recompacts.
	var conCount int
	if !incremental {
		repoTokens := repoTopicSet(repo)
		tally.markCurated(repo.Topics)
		cons, err := g.client.Contributors(ctx, owner, name)
		if e := g.usable(full, "contributors", len(cons), err); e != nil {
			return nil, nil, Position{}, fmt.Errorf("contributors: %w", e)
		}
		conCount = len(cons)
		for _, c := range cons {
			// The API types automation accounts; the login-suffix check in
			// bump stays as a backstop for reviews and comments, which carry
			// only a login.
			if c.IsBot() {
				continue
			}
			tally.bump(c.Login, repoTokens, time.Time{})
		}
	}

	var episodes []episode.Episode
	pulls, err := g.client.PullRequests(ctx, owner, name)
	if e := g.usable(full, "pulls", len(pulls), err); e != nil {
		return nil, nil, Position{}, fmt.Errorf("pulls: %w", e)
	}
	for _, pr := range pulls {
		// Pulls come newest-first, so on an incremental run stop at the first one
		// older than the cursor; only changed pulls are folded again.
		if incremental && pr.UpdatedAt.Before(prev.Cursor) {
			break
		}
		// The cursor is inclusive, so the pull requests at it come back every
		// run. One that has not changed since it was counted is left out.
		if seen.counted(fmt.Sprintf("pr:%d", pr.Number), pr.UpdatedAt) {
			continue
		}
		tally.markCurated(pr.LabelNames())
		// Only the labels are paired. A title is prose, and pairing its words
		// would tie a subject to every turn of phrase used near it.
		ties.note(pr.LabelNames(), pr.Author(), full)
		tokens := append(pr.LabelNames(), phraseTokens(pr.Title)...)
		// The list object carries only reviewers still pending; the people who
		// actually reviewed or commented on a merged change, often its real
		// experts, come from the reviews and comments endpoints. Fetch them only
		// for merged pull requests, which are the ones worth the extra calls.
		var helpers []string
		if pr.Merged() {
			helpers = g.pullHelpers(ctx, owner, name, pr.Number)
		}
		took := append([]string{pr.Author()}, pr.Reviewers()...)
		took = append(took, pr.AssigneeLogins()...)
		took = append(took, helpers...)
		// One credit per person per pull request, whatever the mix of roles: an
		// author who was also assigned and commented on their own change was
		// counted three times, which tripled every self-managed ticket.
		people := util.Distinct(took, func(s string) string { return s })
		for _, u := range people {
			tally.bump(u, tokens, pr.UpdatedAt)
		}
		tally.pulls[PullRef{Repo: strings.ToLower(owner + "/" + name), Number: pr.Number}] = people
		if g.opts.Episodes {
			if ep, ok := changeEpisode(owner, name, pr, helpers); ok {
				episodes = append(episodes, ep)
			}
		}
	}

	issues, err := g.client.Issues(ctx, owner, name, prev.Cursor)
	if errors.Is(err, github.ErrNotFound) {
		// A repository with its issue tracker turned off has no issues to
		// read. That is a complete read, not a failed one.
		issues, err = nil, nil
	}
	if e := g.usable(full, "issues", len(issues), err); e != nil {
		// The pull requests read so far are dropped with the rest. The
		// repository keeps its old position, so the next run reads them again
		// and keeping them now would count them twice.
		return nil, nil, Position{}, fmt.Errorf("issues: %w", e)
	}
	var issueCount int
	for _, is := range issues {
		if is.IsPullRequest() {
			continue
		}
		if seen.counted(fmt.Sprintf("issue:%d", is.Number), is.UpdatedAt) {
			continue
		}
		issueCount++
		tally.markCurated(is.LabelNames())
		ties.note(is.LabelNames(), is.Author(), full)
		tokens := append(is.LabelNames(), phraseTokens(is.Title)...)
		// One credit per person per issue: a self-assigned issue is one piece
		// of work, not two.
		took := append([]string{is.Author()}, is.AssigneeLogins()...)
		for _, u := range util.Distinct(took, func(s string) string { return s }) {
			tally.bump(u, tokens, is.UpdatedAt)
		}
	}

	// CODEOWNERS is a whole-file snapshot with no since filter and rarely changes,
	// so like contributors it is skipped on an incremental run to avoid folding
	// its weight twice.
	var codeOwnerRecords []Record
	if !incremental {
		if content := g.codeOwners(ctx, owner, name); content != nil {
			recs, err := parseCodeOwners(ctx, bytes.NewReader(content))
			switch {
			case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
				return nil, nil, Position{}, err
			case err != nil:
				fmt.Fprintf(g.opts.Log, "github: %s CODEOWNERS parse failed: %v\n", full, err)
			default:
				codeOwnerRecords = remapCodeOwners(recs)
			}
		}
	}
	fmt.Fprintf(g.opts.Log, "github: indexed %s (%d contributors, %d pulls, %d issues)\n",
		full, conCount, len(pulls), issueCount)
	pos := seen.position()
	if pos.Cursor.IsZero() {
		// A repository with no dated pull request or issue was still read to
		// the end. Without a cursor the next run would read it whole again and
		// count its contributors twice.
		pos.Cursor = seen.limit
	}
	return codeOwnerRecords, episodes, pos, nil
}

// usable reports whether a listing error is tolerable. A nil error and a
// truncation, whose partial results are still usable, both return nil; the
// truncation is logged. Any other error is returned so the repo is skipped.
func (g *GitHub) usable(full, what string, n int, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, github.ErrTruncated) {
		fmt.Fprintf(g.opts.Log, "github: %s %s truncated, indexing %d\n", full, what, n)
		return nil
	}
	return err
}

// repoList resolves the explicit repos plus any from the org, capped.
func (g *GitHub) repoList(ctx context.Context) ([]string, error) {
	repos := append([]string(nil), g.opts.Repos...)
	if g.opts.Org != "" {
		orgRepos, err := g.client.OrgRepos(ctx, g.opts.Org)
		if err != nil {
			return nil, fmt.Errorf("github org %s: %w", g.opts.Org, err)
		}
		for i, r := range orgRepos {
			if g.opts.MaxRepos > 0 && i >= g.opts.MaxRepos {
				fmt.Fprintf(g.opts.Log, "github: stopping at %d org repos (cap)\n", g.opts.MaxRepos)
				break
			}
			repos = append(repos, r.FullName)
		}
	}
	if len(repos) == 0 {
		return nil, ErrNoRepos
	}
	return repos, nil
}

// resolveAccounts looks up each login's profile when email resolution is on.
// repoWorkers bounds how many repositories are fetched at once. GitHub's
// secondary rate limits punish aggressive concurrency, so this stays modest.
const repoWorkers = 6

// lockedWriter serializes writes so concurrent per-repo logging cannot
// interleave or race on the underlying writer.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// Write locks around the underlying write.
func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// accountWorkers bounds how many profile lookups run at once. Email resolution
// is one request per contributor, so a large org means hundreds of them;
// running a bounded batch concurrently turns minutes of serial waiting into
// seconds without tripping GitHub's secondary rate limits.
const accountWorkers = 8

func (g *GitHub) resolveAccounts(ctx context.Context, logins map[string]map[string]int) map[string]github.Account {
	accounts := make(map[string]github.Account)
	if !g.opts.ResolveEmails {
		return accounts
	}
	progress := util.ProgressWriter(g.opts.Log, "github: resolved emails for", 100)
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		done int
	)
	sem := make(chan struct{}, accountWorkers)
	for login := range logins {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return accounts
		}
		wg.Add(1)
		go func(login string) {
			defer wg.Done()
			defer func() { <-sem }()
			a, err := g.client.Account(ctx, login)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				accounts[login] = a
			}
			done++
			progress.Report(done)
		}(login)
	}
	wg.Wait()
	return accounts
}

// codeOwners returns the first CODEOWNERS file found in the repo, or nil. A
// repository without the file is normal and stays quiet; any other failure,
// such as a rate limit or a server error, is reported, because it silently
// costs the run every declared owner otherwise.
func (g *GitHub) codeOwners(ctx context.Context, owner, name string) []byte {
	for _, p := range []string{"CODEOWNERS", ".github/CODEOWNERS", "docs/CODEOWNERS"} {
		content, err := g.client.FileContents(ctx, owner, name, p)
		switch {
		case err == nil:
			return content
		case errors.Is(err, github.ErrNotFound):
		default:
			fmt.Fprintf(g.opts.Log,
				"github: %s/%s: %s unreadable, declared ownership from it is missing: %v\n",
				owner, name, p, err)
		}
	}
	return nil
}

// githubPersonRecord builds a person record. The handle always keys the
// record and a resolved email travels with it, so the indexer joins the two:
// without that join a GitHub login resolves to nobody, and work done under a
// handle cannot be found by the person who did it.
func githubPersonRecord(login string, topics []string, a github.Account) Record {
	rec := Record{
		Kind: KindPerson, Source: "github", Weight: 1, Topics: topics,
		PersonID: "github:" + strings.ToLower(login),
	}
	if a.Email != "" {
		rec.Email = util.NormalizeEmail(a.Email)
	}
	rec.Name = a.Name
	if rec.Name == "" {
		rec.Name = "@" + login
	}
	return rec
}

// remapCodeOwners rewrites a repo's own CODEOWNERS @login owners into the github
// identity namespace, so a login that also authored pull requests or issues in
// the same repo merges into one person. Team owners (@org/team) and email owners
// keep their own contact entries.
func remapCodeOwners(recs []Record) []Record {
	for i := range recs {
		login, ok := strings.CutPrefix(recs[i].Name, "@")
		if !ok || strings.Contains(login, "/") {
			continue
		}
		recs[i].PersonID = "github:" + strings.ToLower(login)
	}
	return recs
}

// pullHelpers returns the people who actually reviewed or commented on a merged
// pull request, beyond the author, requested reviewers, and assignees the list
// object already carries. A fetch failure costs those names, not the pull
// request, so it is logged and the rest proceeds.
func (g *GitHub) pullHelpers(ctx context.Context, owner, repo string, number int) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(logins []string, err error, what string) {
		if err != nil {
			fmt.Fprintf(g.opts.Log, "github: %s for %s/%s#%d: %v\n", what, owner, repo, number, err)
			return
		}
		for _, l := range logins {
			if l == "" || seen[l] {
				continue
			}
			seen[l] = true
			out = append(out, l)
		}
	}
	revs, err := g.client.PullReviewers(ctx, owner, repo, number)
	add(revs, err, "reviews")
	comments, err := g.client.PullCommenters(ctx, owner, repo, number)
	add(comments, err, "comments")
	return out
}

// repoTopicSet derives a repo's topic tags from its GitHub topics and the words
// of its name and description.
func repoTopicSet(repo github.Repo) []string {
	out := append([]string(nil), repo.Topics...)
	out = append(out, titleTokens(repo.Name)...)
	out = append(out, titleTokens(repo.Description)...)
	return out
}

// titleTokens splits text into lowercase topic words, dropping short tokens,
// generic code words, and common title filler.
func titleTokens(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		if len(f) >= 3 && !codeStop[f] && !noiseWords[f] {
			out = append(out, f)
		}
	}
	return out
}

// phraseTokens returns the tokens of s plus the two-word phrases formed by
// adjacent surviving tokens, hyphenated. Expertise is often a compound
// ("billing retries", "incident response"), and tokenizing alone shatters it
// into words that each look like a subject of their own. Emitting the phrase
// beside its words lets the compound accumulate its own weight while the single
// words still match a question that only uses one of them.
func phraseTokens(s string) []string {
	words := titleTokens(s)
	if len(words) < 2 {
		return words
	}
	out := make([]string, 0, len(words)*2-1)
	out = append(out, words...)
	for i := 0; i+1 < len(words); i++ {
		out = append(out, words[i]+"-"+words[i+1])
	}
	return out
}

// splitCurated divides topic tokens into the ones some source stated outright
// and the ones inferred from prose, so the index can tell a declared subject
// apart from a word that happened to appear in a title. Order is preserved and
// repeats are kept, because repetition is how volume of work raises a score.
func splitCurated(tokens []string, curated map[string]bool) (strong, weak []string) {
	for _, t := range tokens {
		if curated[strings.ToLower(strings.TrimSpace(t))] {
			strong = append(strong, t)
			continue
		}
		weak = append(weak, t)
	}
	return strong, weak
}

// expandTopics turns per-token counts into a topic slice with each token
// repeated by its capped count, so volume of work raises a person's score.
func expandTopics(counts map[string]int) []string {
	tokens := slices.Sorted(maps.Keys(counts))

	var out []string
	for _, t := range tokens {
		n := topicRepeat(counts[t])
		for range n {
			out = append(out, t)
		}
	}
	return out
}

// topicRepeat turns a raw count of how often somebody touched a subject into
// how many times it is worth repeating: one for one while the evidence is
// thin, then logarithmically, so sustained work always outweighs incidental
// work without any amount of it running away with the score.
func topicRepeat(count int) int {
	if count <= maxTopicWeight {
		return count
	}
	n := int(math.Round(maxTopicWeight * (1 + math.Log(float64(count)/maxTopicWeight))))
	return min(n, maxTopicRepeat)
}

// splitRepo splits "owner/name" into its parts.
func splitRepo(full string) (owner, name string, ok bool) {
	owner, name, ok = strings.Cut(strings.TrimSpace(full), "/")
	if !ok || owner == "" || name == "" {
		return "", "", false
	}
	return owner, name, true
}
