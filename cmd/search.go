package cmd

import (
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dcadolph/whotoask/internal/engine"
	"github.com/dcadolph/whotoask/internal/render"
)

// newSearchCmd builds the search command, a direct lookup of people and channels
// by name, email, title, team, or topic. It differs from ask, which orders people
// by their record on a subject; search just finds an entity by what it is called.
func newSearchCmd(opts *options) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Find people and channels by name, email, title, team, or topic",
		Long: `Find people and channels whose name, email, title, team, or topic contains the
query, most direct match first. This is a direct lookup. Use ask to find who to
ask about a subject.

Examples:
  whotoask search kim
  whotoask search payments
  whotoask search "staff engineer"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ix, err := opts.loadIndex(cmd)
			if err != nil {
				return noIndexError(err)
			}
			query := strings.Join(args, " ")
			results := engine.New(ix, opts.pol).Search(query, limit)
			v := map[string]any{"query": query, "results": results}
			return opts.render(cmd.OutOrStdout(), v, func(w io.Writer, s style) {
				render.RenderSearch(w, query, results, s)
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "Maximum results to return.")
	return cmd
}
