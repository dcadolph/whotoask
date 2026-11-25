package cmd

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/dcadolph/whotoask/internal/render"
)

// style is the render package's color switch, kept under its short name for
// the command callbacks that receive it.
type style = render.Style

// plural returns "s" unless n is one.
func plural(n int) string { return render.Plural(n) }

// isTerminal reports whether w writes to an interactive terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// render writes v as human-readable text when the destination is a terminal,
// and as JSON when the output is piped or when --json or --pretty asks for it.
// --human forces the human form even through a pipe, for reading in a pager.
// The human function is handed a style that colors only at a real terminal.
func (o *options) render(w io.Writer, v any, human func(io.Writer, style)) error {
	if o.jsonOut || o.pretty {
		return writeJSON(w, v, o.pretty)
	}
	tty := isTerminal(w)
	if o.humanOut || tty {
		human(w, style{On: tty})
		return nil
	}
	return writeJSON(w, v, false)
}

// notForLine closes every human rendering of a knowledge-risk finding. It is
// the same promise the README makes, word for word.
const notForLine = "WhoToAsk reads the record your team's tools keep. " +
	"For a question, it lists the people and channels whose record matches, best match first. " +
	"For a subject, it shows how that record is split among people. " +
	"Both describe the record, not the person. " +
	"A weak or missing match is not evidence about anyone, because work done in meetings, " +
	"in pairing, or in tools WhoToAsk does not read leaves no record. " +
	notForSentence + " " +
	"A subject with one person on record is a sign the team needs a second person on it."

// notForSentence is the one sentence every surface that reports on people
// carries, the assessment summary and the MCP tool descriptions among them.
const notForSentence = "Its findings must not be used for performance reviews, layoffs, " +
	"or any other employment decision."

// renderNotFor writes notForLine as the last line of a finding.
func renderNotFor(w io.Writer, s style) {
	fmt.Fprintf(w, "\n%s\n", s.Dim(notForLine))
}
