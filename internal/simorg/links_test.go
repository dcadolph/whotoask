package simorg

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dcadolph/whotoask/internal/episode"
	"github.com/dcadolph/whotoask/internal/index"
)

// TestEpisodesStoreNoLocalLinks checks a simulated company's conversations
// never link to the local server the demo builds against, which is gone the
// moment the build ends, and that Jira links land on the stable placeholder.
func TestEpisodesStoreNoLocalLinks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Build func(string) (*index.Index, error)
		Eps   func(*index.Index) (*episode.Store, error)
	}{{ // Test 0: The small company.
		Build: BuildIndex, Eps: BuildEpisodes,
	}, { // Test 1: The large company.
		Build: BuildBigIndex, Eps: BuildBigEpisodes,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ix, err := test.Build(t.TempDir())
			if err != nil {
				t.Fatalf("build index: %v", err)
			}
			store, err := test.Eps(ix)
			if err != nil {
				t.Fatalf("build episodes: %v", err)
			}
			jira := 0
			for _, ep := range store.All() {
				local := strings.Contains(ep.Permalink, "127.0.0.1") ||
					strings.Contains(ep.Permalink, "localhost")
				if local {
					t.Errorf("episode %s links to a local server: %s", ep.ID, ep.Permalink)
				}
				if strings.HasPrefix(ep.Permalink, demoJiraURL+"/browse/") {
					jira++
				}
			}
			if jira == 0 {
				t.Errorf("no Jira link under %s", demoJiraURL)
			}
		})
	}
}

// TestStableLinks checks only links under the local base are rewritten.
func TestStableLinks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantLink string
		In       string
	}{{ // Test 0: A local Jira link moves to the placeholder.
		In: "http://127.0.0.1:5000/browse/DAT-1", WantLink: "https://jira.example.com/browse/DAT-1",
	}, { // Test 1: Any other link is left alone.
		In: "https://github.com/corp/webapp/pull/88", WantLink: "https://github.com/corp/webapp/pull/88",
	}, { // Test 2: No link stays none.
		In: "", WantLink: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			eps := []episode.Episode{{Permalink: test.In}}
			stableLinks(eps, "http://127.0.0.1:5000/")
			if eps[0].Permalink != test.WantLink {
				t.Errorf("link = %q, want %q", eps[0].Permalink, test.WantLink)
			}
		})
	}
}
