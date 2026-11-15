package connector

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/dcadolph/whotoask/internal/slack"
)

// TestSlackOldest verifies the history window floor is used by default and a
// more recent mark wins, so an incremental read narrows to new messages while a
// stale mark never widens the read past the window.
func TestSlackOldest(t *testing.T) {
	t.Parallel()
	window := "1700000000.000000"
	tests := []struct {
		Mark       string
		WantResult string
	}{{ // Test 0: A mark inside the window is used exactly as it was written.
		Mark: "1700000500.000123", WantResult: "1700000500.000123",
	}, { // Test 1: A mark older than the window keeps the window floor.
		Mark: "1600000000.000000", WantResult: window,
	}, { // Test 2: No mark keeps the window floor.
		Mark: "", WantResult: window,
	}, { // Test 3: A mark that does not parse keeps the window floor.
		Mark: "soon", WantResult: window,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := slackOldest(window, test.Mark); got != test.WantResult {
				t.Errorf("slackOldest(%q) = %q, want %q", test.Mark, got, test.WantResult)
			}
		})
	}
}

// TestSlackTSAfter verifies timestamps a microsecond apart still order, which a
// float comparison at this magnitude does not guarantee.
func TestSlackTSAfter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		A, B       string
		WantResult bool
	}{{ // Test 0: One microsecond later is later.
		A: "1712345678.000101", B: "1712345678.000100", WantResult: true,
	}, { // Test 1: Equal is not later.
		A: "1712345678.000100", B: "1712345678.000100", WantResult: false,
	}, { // Test 2: A later second wins over a larger fraction.
		A: "1712345679.000000", B: "1712345678.999999", WantResult: true,
	}, { // Test 3: Anything that parses is later than nothing.
		A: "1712345678.000100", B: "", WantResult: true,
	}, { // Test 4: Nothing is never later.
		A: "", B: "1712345678.000100", WantResult: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := slackTSAfter(test.A, test.B); got != test.WantResult {
				t.Errorf("slackTSAfter(%q, %q) = %v, want %v", test.A, test.B, got, test.WantResult)
			}
		})
	}
}

// slackTestServer serves a small workspace for the per-channel resume tests.
// Each channel's history comes from history, which sees the oldest bound the
// connector asked with and may answer with a status other than 200.
func slackTestServer(
	t *testing.T, channels []string, history func(channel, oldest string) (status int, body string),
) *slack.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/users.list", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"ok":true,"members":[{"id":"U1","profile":{"real_name":"Jane","email":"jane@x.com"}}]}`)
	})
	mux.HandleFunc("/conversations.list", func(w http.ResponseWriter, _ *http.Request) {
		var list []string
		for _, id := range channels {
			list = append(list, fmt.Sprintf(`{"id":%q,"name":%q,"is_member":true}`, id, strings.ToLower(id)))
		}
		io.WriteString(w, `{"ok":true,"channels":[`+strings.Join(list, ",")+`]}`)
	})
	mux.HandleFunc("/conversations.history", func(w http.ResponseWriter, r *http.Request) {
		status, body := history(r.FormValue("channel"), r.FormValue("oldest"))
		if status != http.StatusOK {
			// A zero wait keeps the client's retries from slowing the test.
			w.Header().Set("Retry-After", "0")
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return slack.New("xoxb-test", slack.WithBaseURL(srv.URL))
}

// slackHistory renders a history answer holding one message per timestamp.
func slackHistory(stamps ...string) string {
	var msgs []string
	for _, ts := range stamps {
		msgs = append(msgs, fmt.Sprintf(`{"type":"message","user":"U1","text":"retries fixed","ts":%q}`, ts))
	}
	return `{"ok":true,"has_more":false,"messages":[` + strings.Join(msgs, ",") + `]}`
}

// TestSlackResumesEachChannelFromItsOwnMark verifies an incremental read asks
// each channel from that channel's mark, reads a channel with no mark over the
// whole window, and reports the exact newest timestamp as the next mark. One
// time for the whole workspace lost a message posted to a channel already read
// while a later channel was still being read.
func TestSlackResumesEachChannelFromItsOwnMark(t *testing.T) {
	t.Parallel()
	asked := map[string]string{}
	client := slackTestServer(t, []string{"C1", "C2", "C3"}, func(channel, oldest string) (int, string) {
		asked[channel] = oldest
		switch channel {
		case "C1":
			return http.StatusOK, slackHistory("9999999990.000200", "9999999990.000100")
		case "C2":
			return http.StatusOK, slackHistory()
		default:
			return http.StatusOK, slackHistory("9999999995.000300")
		}
	})
	src := NewSlackWithClient(client, SlackOptions{Resume: map[string]Position{
		"C1": {Mark: "9999999980.000100"},
		"C2": {Mark: "9999999985.000100"},
	}})
	if _, err := src.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if asked["C1"] != "9999999980.000100" || asked["C2"] != "9999999985.000100" {
		t.Errorf("asked = %v, want each channel asked from its own mark", asked)
	}
	if slackTSAfter(asked["C3"], "9999999980.000000") {
		t.Errorf("C3 asked from %q, want the start of the window since it has no mark", asked["C3"])
	}
	want := map[string]Position{
		"C1": {Mark: "9999999990.000200"},
		"C2": {Mark: "9999999985.000100"},
		"C3": {Mark: "9999999995.000300"},
	}
	if diff := cmp.Diff(want, src.Positions(), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("positions (-want +got):\n%s", diff)
	}
	if len(src.Skipped()) != 0 {
		t.Errorf("skipped = %v, want none", src.Skipped())
	}
}

// TestSlackSkippedChannelKeepsItsPlace verifies a channel that fails with a
// server error or stays rate limited is skipped without losing the channels
// already read, and is left out of the positions so its saved mark does not
// move. Before, one such failure either threw away the whole read or let the
// cursor step over the channel for good.
func TestSlackSkippedChannelKeepsItsPlace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Status int
	}{{ // Test 0: A server error on one channel.
		Status: http.StatusServiceUnavailable,
	}, { // Test 1: Rate limiting that outlasts the retries on one channel.
		Status: http.StatusTooManyRequests,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			client := slackTestServer(t, []string{"C1", "C2", "C3"}, func(channel, _ string) (int, string) {
				if channel == "C2" {
					return test.Status, `{"ok":false}`
				}
				return http.StatusOK, slackHistory("9999999990.000100")
			})
			var log bytes.Buffer
			src := NewSlackWithClient(client, SlackOptions{
				Log:    &log,
				Resume: map[string]Position{"C2": {Mark: "9999999985.000100"}},
			})
			recs, err := src.Fetch(context.Background())
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			var channels int
			for _, r := range recs {
				if r.Kind == KindChannel {
					channels++
				}
			}
			if channels != 2 {
				t.Errorf("channel records = %d, want the two channels that were read", channels)
			}
			positions := src.Positions()
			if _, moved := positions["C2"]; moved {
				t.Errorf("positions = %v, want no entry for the skipped channel", positions)
			}
			if len(positions) != 2 {
				t.Errorf("positions = %v, want the two channels that were read", positions)
			}
			if diff := cmp.Diff([]string{"c2"}, src.Skipped()); diff != "" {
				t.Errorf("skipped (-want +got):\n%s", diff)
			}
			if !strings.Contains(log.String(), "the next refresh picks each one up where it stopped") {
				t.Errorf("log = %q, want it to say the next refresh resumes the channel", log.String())
			}
		})
	}
}

// TestSlackNothingReadIsAnError verifies a run where every channel failed
// reports the failure instead of saving an empty read as a success.
func TestSlackNothingReadIsAnError(t *testing.T) {
	t.Parallel()
	client := slackTestServer(t, []string{"C1", "C2"}, func(string, string) (int, string) {
		return http.StatusServiceUnavailable, `{"ok":false}`
	})
	src := NewSlackWithClient(client, SlackOptions{})
	if _, err := src.Fetch(context.Background()); err == nil {
		t.Error("Fetch returned no error when no channel could be read")
	}
	if len(src.Positions()) != 0 {
		t.Errorf("positions = %v, want none", src.Positions())
	}
}

// TestSlackUnreadableChannelIsNotSkipped verifies a channel the bot is not in
// is left without a position and is not reported as skipped, so it is read
// whole once the bot is invited and the run still counts as complete.
func TestSlackUnreadableChannelIsNotSkipped(t *testing.T) {
	t.Parallel()
	client := slackTestServer(t, []string{"C1", "C2"}, func(channel, _ string) (int, string) {
		if channel == "C2" {
			return http.StatusOK, `{"ok":false,"error":"not_in_channel"}`
		}
		return http.StatusOK, slackHistory("9999999990.000100")
	})
	src := NewSlackWithClient(client, SlackOptions{})
	if _, err := src.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, ok := src.Positions()["C2"]; ok || len(src.Skipped()) != 0 {
		t.Errorf("positions = %v, skipped = %v, want C2 in neither", src.Positions(), src.Skipped())
	}
}
