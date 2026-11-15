package connector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dcadolph/whotoask/internal/episode"
	"github.com/dcadolph/whotoask/internal/model"
	"github.com/dcadolph/whotoask/internal/slack"
	"github.com/dcadolph/whotoask/internal/util"
)

// Default Slack ingest bounds. Standard depth pulls roughly six months of
// history, capped per channel so one busy channel cannot dominate a run.
const (
	// defaultSinceDays is the history window in days.
	defaultSinceDays = 180
	// defaultMaxMessages caps messages read per channel.
	defaultMaxMessages = 5000
	// maxAuthorText caps stored text per author per channel, in bytes.
	maxAuthorText = 8000
	// sampleBudget caps stored channel sample text, in bytes.
	sampleBudget = 4000
)

// SlackOptions configures the Slack connector.
type SlackOptions struct {
	// IncludePrivate also ingests private channels the token can read.
	IncludePrivate bool
	// JoinPublic makes the bot join each public channel it is not already in
	// before reading it, so a whole workspace can be indexed without inviting
	// the bot by hand. It needs the channels:join scope and posts a join notice
	// in each channel it enters. Private channels still require an invite.
	JoinPublic bool
	// SinceDays bounds history age; zero uses the default.
	SinceDays int
	// MaxMessages caps messages per channel; zero uses the default.
	MaxMessages int
	// MaxChannels caps channels processed; zero means all.
	MaxChannels int
	// Episodes records the conversations behind the messages, so whotoask can
	// point back at the discussion that solved something.
	Episodes bool
	// MaxEpisodesPerChannel caps episodes kept per channel; zero uses the
	// default.
	MaxEpisodesPerChannel int
	// Archive retains the content of each conversation, not just a link to
	// it. It is the only setting that reads thread replies.
	Archive bool
	// MaxArchiveMessages caps retained messages per conversation; zero uses
	// the default.
	MaxArchiveMessages int
	// Resume holds where an earlier read stopped in each channel, keyed by
	// channel ID, for an incremental re-index. A channel in it is read from its
	// mark, and a channel missing from it is read over the whole SinceDays
	// window, which is how a channel the bot was just invited to gets its
	// history. Nil reads every channel over the window. Identity records (users)
	// carry no weight, so only the message-derived topics narrow to the delta.
	Resume map[string]Position
	// Log receives progress lines; nil discards them.
	Log io.Writer
}

// withDefaults fills zero fields with defaults.
func (o SlackOptions) withDefaults() SlackOptions {
	if o.SinceDays <= 0 {
		o.SinceDays = defaultSinceDays
	}
	if o.MaxMessages <= 0 {
		o.MaxMessages = defaultMaxMessages
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	return o
}

// Slack is a Source that ingests workspace users, channels, and recent history.
type Slack struct {
	// client calls the Slack Web API.
	client *slack.Client
	// opts holds the resolved ingest bounds.
	opts SlackOptions
	// episodes holds the conversations seen by the last Fetch.
	episodes []episode.Episode
	// positions holds where the last Fetch stopped in each channel it read.
	positions map[string]Position
	// skipped names the channels the last Fetch could not read.
	skipped []string
}

// Positions returns where the most recent Fetch stopped in each channel it read
// to the end, keyed by channel ID.
func (s *Slack) Positions() map[string]Position { return clonePositions(s.positions) }

// Skipped names the channels the most recent Fetch could not read because of
// rate limiting or an outage. A channel the bot is not a member of is not one
// of them: it has nothing to resume, and is read whole once the bot is invited.
func (s *Slack) Skipped() []string { return slices.Clone(s.skipped) }

// NewSlack returns a Slack connector authenticating with token.
func NewSlack(token string, opts SlackOptions) *Slack {
	return &Slack{client: slack.New(token), opts: opts.withDefaults()}
}

// NewSlackWithClient returns a Slack connector using a preconfigured client.
// Tests use it to inject a client pointed at a mock server.
func NewSlackWithClient(client *slack.Client, opts SlackOptions) *Slack {
	if client == nil {
		panic("connector: NewSlackWithClient requires a non-nil client")
	}
	return &Slack{client: client, opts: opts.withDefaults()}
}

// Ping verifies the token with a cheap auth.test call, so a wizard can confirm
// credentials before committing to a full index.
func (s *Slack) Ping(ctx context.Context) error {
	_, err := s.client.AuthTest(ctx)
	return err
}

// Fetch reads users, channels, and bounded history, returning person and
// channel records. Person identity joins other sources by email. Deactivated
// members come through like anyone else: their history is exactly what a
// continuity question is about, and the export path keeps them too.
func (s *Slack) Fetch(ctx context.Context) ([]Record, error) {
	// The workspace URL turns a channel id into a link and a message timestamp
	// into a permalink, and the workspace ID turns a user into a direct message
	// link, with no further calls. Losing them costs links, not data, so a
	// failure here is logged and the read goes on. A bad token still fails at
	// the users call below.
	auth, err := s.client.AuthTest(ctx)
	if err != nil {
		fmt.Fprintf(s.opts.Log, "slack: no workspace url, person, channel, and episode links unavailable: %v\n", err)
		auth = slack.Auth{}
	}
	workspaceURL := auth.URL

	users, err := s.client.Users(ctx)
	switch {
	case errors.Is(err, slack.ErrTruncated):
		fmt.Fprintf(s.opts.Log, "slack: %v; members past that point are missing from this index\n", err)
	case err != nil:
		return nil, fmt.Errorf("slack users: %w", err)
	}
	byID := make(map[string]slack.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}

	var records []Record
	noEmail := 0
	for _, u := range users {
		if notPerson(u) || u.Profile.RealName == "" && u.Profile.Email == "" {
			continue
		}
		if u.Profile.Email == "" {
			noEmail++
		}
		records = append(records, personRecord(u, auth.TeamID))
	}
	if noEmail > 0 {
		// A user with no email keys on the opaque slack:UID and joins nothing,
		// so their Slack activity splits from the same person's git, GitHub, and
		// Jira work, and recall --me finds none of their Slack episodes. Surface
		// the count so the missing scope is visible rather than silently halving
		// the graph.
		fmt.Fprintf(s.opts.Log,
			"slack: %d of %d members have no email; the token lacks users:read.email, so their "+
				"Slack activity will not merge with the same people from other sources. Add the "+
				"users:read.email scope and reinstall to link them\n", noEmail, len(records))
	}

	types := "public_channel"
	if s.opts.IncludePrivate {
		types = "public_channel,private_channel"
	}
	channels, err := s.client.Channels(ctx, types)
	switch {
	case errors.Is(err, slack.ErrTruncated):
		fmt.Fprintf(s.opts.Log, "slack: %v; channels past that point were not read\n", err)
	case err != nil:
		return nil, fmt.Errorf("slack channels: %w", err)
	}
	fmt.Fprintf(s.opts.Log, "slack: %d users, %d channels\n", len(users), len(channels))

	if s.opts.Episodes {
		s.episodes = nil
	}
	window := slackTS(time.Now().AddDate(0, 0, -s.opts.SinceDays))
	s.positions = make(map[string]Position)
	s.skipped = nil
	skipped, read, notInChannel := 0, 0, 0
	joined := 0
	// maxConsecFailed stops a run only when the whole workspace is hard rate
	// limited or unreachable, not on a single slow channel.
	const maxConsecFailed = 5
	consecFailed := 0
	var lastFailure error
	if s.opts.MaxChannels > 0 && len(channels) > s.opts.MaxChannels {
		fmt.Fprintf(s.opts.Log, "slack: stopping at %d channels (cap)\n", s.opts.MaxChannels)
		channels = channels[:s.opts.MaxChannels]
	}
	for i, ch := range channels {
		// Self-join a public channel the bot is not in yet, so the whole
		// workspace indexes without hand invites. A private channel cannot be
		// self-joined; only a member can add the bot there.
		if s.opts.JoinPublic && !ch.IsPrivate && !ch.IsMember {
			if err := s.client.JoinChannel(ctx, ch.ID); err != nil {
				fmt.Fprintf(s.opts.Log, "slack: could not join #%s: %v\n", ch.Name, err)
			} else {
				joined++
			}
		}
		// Each channel resumes from its own mark. One time for the whole
		// workspace stepped over a message posted to a channel already read
		// while a later channel was still being read.
		oldest := slackOldest(window, s.opts.Resume[ch.ID].Mark)
		msgs, err := s.client.History(ctx, ch.ID, oldest, s.opts.MaxMessages)
		if err != nil && ctx.Err() != nil {
			return nil, fmt.Errorf("slack history for %s: %w", ch.Name, err)
		}
		if err != nil && errors.Is(err, slack.ErrAPI) && !errors.Is(err, slack.ErrRateLimited) {
			// Any per-channel API error, such as not_in_channel on a public
			// channel the bot was never invited to, is skipped so one
			// unreadable channel does not cost the whole run.
			consecFailed = 0
			skipped++
			if strings.Contains(err.Error(), "not_in_channel") {
				notInChannel++
			}
			fmt.Fprintf(s.opts.Log, "slack: skipping #%s: %v\n", ch.Name, err)
			continue
		}
		if err != nil {
			// The retry budget is large, so reaching here means this channel
			// stayed throttled or unreachable for many minutes. Skip it and keep
			// going rather than abandoning every later channel or throwing away
			// what was already read. Its mark does not move, so the next run
			// reads it from where it really stopped. Only when many channels in
			// a row fail is the whole workspace out of reach, and then stopping
			// and keeping what was read beats spinning.
			s.skipped = append(s.skipped, ch.Name)
			consecFailed++
			lastFailure = err
			if errors.Is(err, slack.ErrRateLimited) {
				fmt.Fprintf(s.opts.Log, "slack: still rate limited at #%s after retries; skipping it\n", ch.Name)
			} else {
				fmt.Fprintf(s.opts.Log, "slack: could not read #%s, skipping it: %v\n", ch.Name, err)
			}
			if consecFailed >= maxConsecFailed {
				for _, rest := range channels[i+1:] {
					s.skipped = append(s.skipped, rest.Name)
				}
				fmt.Fprintf(s.opts.Log,
					"slack: %d channels in a row could not be read; stopping with %d read and %d left\n",
					consecFailed, read, len(s.skipped))
				break
			}
			continue
		}
		consecFailed = 0
		s.positions[ch.ID] = Position{Mark: slackNewest(msgs, oldest)}
		if len(msgs) >= s.opts.MaxMessages {
			fmt.Fprintf(s.opts.Log,
				"slack: #%s hit the %d message cap; older messages skipped\n", ch.Name, s.opts.MaxMessages)
		}

		chRec, authorText, authorLatest := channelRecord(ch, msgs, byID, workspaceURL)
		records = append(records, chRec)
		records = append(records, authorTextRecords(authorText, authorLatest, auth.TeamID)...)
		if s.opts.Episodes {
			eps := collectEpisodes(ch, msgs, episodeOpts{
				byID:         byID,
				workspaceURL: workspaceURL,
				max:          s.opts.MaxEpisodesPerChannel,
				archive:      s.opts.Archive,
				maxArchive:   s.opts.MaxArchiveMessages,
			})
			// Read each thread's replies and fold them into the searchable body
			// so recall matches the solution, not just the question. Verbatim
			// retention for quoting stays gated to an archive inside.
			s.enrichThreads(ctx, eps, byID)
			s.episodes = append(s.episodes, eps...)
		}
		read++
		fmt.Fprintf(s.opts.Log, "slack: indexed #%s (%d messages)\n", ch.Name, len(msgs))
	}
	if joined > 0 {
		fmt.Fprintf(s.opts.Log, "slack: joined %d public channels to read them\n", joined)
	}
	if skipped > 0 {
		switch {
		case read == 0 && notInChannel == skipped:
			// The token works and lists channels, but the bot is in none of
			// them, so history is refused everywhere. This is the first-run
			// case: the app is installed but not yet invited anywhere.
			fmt.Fprintf(s.opts.Log,
				"slack: the bot is not a member of any channel, so no messages could be read. "+
					"In Slack, invite it with `/invite @whotoask` in the channels you want indexed, "+
					"then run this again\n")
		case read == 0:
			// Not a membership problem, so the token cannot read history at
			// all: the scope is missing.
			fmt.Fprintf(s.opts.Log,
				"slack: no channel could be read. The bot token is likely missing the "+
					"channels:history scope (and groups:history for private channels). "+
					"Add it in your Slack app config and reinstall\n")
		default:
			fmt.Fprintf(s.opts.Log,
				"slack: skipped %d channels the bot is not in; invite it to the ones that matter\n", skipped)
		}
	}
	if read == 0 && len(s.skipped) > 0 {
		return nil, fmt.Errorf("slack: no channel could be read: %w", lastFailure)
	}
	if len(s.skipped) > 0 {
		fmt.Fprintf(s.opts.Log,
			"slack: %d channels could not be read this time; the next refresh picks each one up "+
				"where it stopped\n", len(s.skipped))
	}
	return records, nil
}

// personRecord builds a person record from a Slack user. The record carries
// the Slack user ID as its identifier and the email alongside it, so the
// indexer joins the two. Without that join a Slack user ID resolves to nobody,
// and the bot cannot tell which person is asking. With the workspace ID known
// it also carries the link that opens a direct message with them.
func personRecord(u slack.User, teamID string) Record {
	return Record{
		Kind:     KindPerson,
		Source:   "slack",
		Weight:   1,
		PersonID: "slack:" + u.ID,
		Name:     u.Profile.RealName,
		Email:    u.Profile.Email,
		Title:    u.Profile.Title,
		Link:     dmLink(teamID, u.ID),
		Account:  accountStatus(u),
	}
}

// accountStatus reports a Slack account as active or deactivated. Slack lists
// every member with this flag, so its status is always known.
func accountStatus(u slack.User) model.AccountStatus {
	if u.Deleted {
		return model.AccountDeactivated
	}
	return model.AccountActive
}

// dmLink is the slack:// deep link that opens a direct message with userID in
// the workspace teamID, empty when either is unknown.
func dmLink(teamID, userID string) string {
	if teamID == "" || userID == "" {
		return ""
	}
	return "slack://user?team=" + url.QueryEscape(teamID) + "&id=" + url.QueryEscape(userID)
}

// channelRecord builds a channel record, the per-author message text mined
// for personal affinity, and each author's latest message time. System and
// bot messages are skipped.
func channelRecord(
	ch slack.Channel, msgs []slack.Message, byID map[string]slack.User, workspaceURL string,
) (Record, map[string]string, map[string]time.Time) {
	var members []string
	seen := make(map[string]bool)
	authorText := make(map[string]string)
	authorLatest := make(map[string]time.Time)
	var latest time.Time
	var sample strings.Builder

	for _, m := range msgs {
		if m.Subtype != "" || m.User == "" || m.BotID != "" {
			continue
		}
		pid := slackUserRef(m.User, byID)
		if pid == "" {
			continue
		}
		if !seen[pid] {
			seen[pid] = true
			members = append(members, pid)
		}
		if len(authorText[pid]) < maxAuthorText {
			authorText[pid] += " " + m.Text
		}
		if ts := slackTime(m.TS); !ts.IsZero() {
			if ts.After(authorLatest[pid]) {
				authorLatest[pid] = ts
			}
			if ts.After(latest) {
				latest = ts
			}
		}
		if sample.Len() < sampleBudget {
			sample.WriteString(" ")
			sample.WriteString(m.Text)
		}
	}

	link := ""
	if workspaceURL != "" && ch.ID != "" {
		link = strings.TrimSuffix(workspaceURL, "/") + "/archives/" + ch.ID
	}
	rec := Record{
		Kind:    KindChannel,
		Source:  "slack",
		Weight:  1,
		Name:    ch.Name,
		Link:    link,
		Title:   ch.Topic.Value,
		Text:    strings.TrimSpace(ch.Purpose.Value + " " + sample.String()),
		Members: members,
		Time:    latest,
	}
	return rec, authorText, authorLatest
}

// authorTextRecords turns per-author mined text into person records, in
// sorted author order. Map order here was the one nondeterminism in a Slack
// read: same workspace, differently ordered records, and byte-for-byte
// reproducibility is a product claim. An author known only by Slack user ID
// carries the direct message link when teamID is known; an author keyed by
// email gets it from their user record.
func authorTextRecords(authorText map[string]string, authorLatest map[string]time.Time, teamID string) []Record {
	pids := slices.Sorted(maps.Keys(authorText))
	out := make([]Record, 0, len(pids))
	for _, pid := range pids {
		out = append(out, Record{
			Kind: KindPerson, Source: "slack", Weight: 1, PersonID: pid, Text: authorText[pid],
			Time: authorLatest[pid], Link: authorLink(pid, teamID),
		})
	}
	return out
}

// authorLink is the direct message link for an author keyed by Slack user ID,
// empty for an author keyed by email.
func authorLink(pid, teamID string) string {
	uid, ok := strings.CutPrefix(pid, "slack:")
	if !ok {
		return ""
	}
	return dmLink(teamID, uid)
}

// slackTime parses a Slack epoch timestamp such as "1712345678.000100",
// returning the zero time when it does not parse.
func slackTime(ts string) time.Time {
	sec, err := strconv.ParseFloat(ts, 64)
	if err != nil || sec <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(sec), 0).UTC()
}

// slackPersonID resolves a stable person ID from a user, preferring email.
func slackPersonID(u slack.User) string {
	if u.Profile.Email != "" {
		return util.NormalizeEmail(u.Profile.Email)
	}
	return "slack:" + u.ID
}

// slackbotID is the user ID Slack gives Slackbot in every workspace. Slackbot is
// not flagged as a bot, so it is named here.
const slackbotID = "USLACKBOT"

// notPerson reports whether a Slack account is not a person to index: a bot or
// Slackbot. A deactivated account is still a person, since the recently departed
// are who a knowledge-continuity read is about.
func notPerson(u slack.User) bool {
	return u.IsBot || u.ID == slackbotID
}

// slackUserRef resolves a message author's user ID to a stable person ID. It
// returns "" for an account that is not a person, so its messages name no one.
func slackUserRef(userID string, byID map[string]slack.User) string {
	if userID == "" || userID == slackbotID {
		return ""
	}
	if u, ok := byID[userID]; ok {
		if notPerson(u) {
			return ""
		}
		return slackPersonID(u)
	}
	return "slack:" + userID
}

// slackOldest returns the oldest bound for one channel's read: the start of the
// history window, or the channel's mark when it is more recent, so an
// incremental read asks only for messages posted after the last one it read.
// Slack treats the bound as exclusive, so the message at the mark is not read
// again. A mark older than the window never widens the read past it.
func slackOldest(window, mark string) string {
	if slackTSAfter(mark, window) {
		return mark
	}
	return window
}

// slackNewest returns the timestamp of the newest message in msgs, or oldest
// when there is none, so a channel with nothing new keeps its place.
func slackNewest(msgs []slack.Message, oldest string) string {
	newest := oldest
	for _, m := range msgs {
		if slackTSAfter(m.TS, newest) {
			newest = m.TS
		}
	}
	return newest
}

// slackTSAfter reports whether Slack timestamp a is later than b. A timestamp
// that does not parse is never later, and anything that parses is later than
// one that does not.
func slackTSAfter(a, b string) bool {
	as, af, ok := slackTSParts(a)
	if !ok {
		return false
	}
	bs, bf, ok := slackTSParts(b)
	if !ok {
		return true
	}
	return as > bs || (as == bs && af > bf)
}

// slackTSParts splits a Slack timestamp such as "1712345678.000100" into whole
// seconds and microseconds. The two halves are compared as integers, since a
// float cannot hold every microsecond at this magnitude and two messages a
// microsecond apart would compare equal.
func slackTSParts(ts string) (sec, micro int64, ok bool) {
	whole, frac, _ := strings.Cut(ts, ".")
	sec, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || sec <= 0 {
		return 0, 0, false
	}
	if frac == "" {
		return sec, 0, true
	}
	frac = (frac + "000000")[:6]
	micro, err = strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return sec, micro, true
}

// slackTS formats a time as a Slack message timestamp, Unix seconds with a
// microsecond suffix.
func slackTS(t time.Time) string {
	return fmt.Sprintf("%d.000000", t.Unix())
}
