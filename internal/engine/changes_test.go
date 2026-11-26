package engine

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/dcadolph/whotoask/internal/index"
)

// TestReportChangesSaysIndexPresence checks the change lines describe the
// index, not employment: a name that moved in or out of the index is never
// reported as somebody joining or leaving.
func TestReportChangesSaysIndexPresence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantLines []string
		In        index.Changes
	}{{ // Test 0: A name added to the index.
		In:        index.Changes{PeopleJoined: []string{"Ann Lee"}},
		WantLines: []string{"  added to index: Ann Lee"},
	}, { // Test 1: A name no longer in the index.
		In:        index.Changes{PeopleLeft: []string{"Bo Ito"}},
		WantLines: []string{"  no longer in index: Bo Ito"},
	}, { // Test 2: Nothing changed prints nothing.
		In: index.Changes{},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var b bytes.Buffer
			reportChanges(&b, test.In)
			got := b.String()
			for _, want := range test.WantLines {
				if !strings.Contains(got, want) {
					t.Errorf("output %q missing %q", got, want)
				}
			}
			for _, banned := range []string{"joined:", "left:"} {
				if strings.Contains(got, banned) {
					t.Errorf("output %q reads as employment: %q", got, banned)
				}
			}
			if len(test.WantLines) == 0 && got != "" {
				t.Errorf("output = %q, want nothing", got)
			}
		})
	}
}
