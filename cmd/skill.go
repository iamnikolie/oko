package cmd

import (
	_ "embed"
	"fmt"

	"github.com/spf13/cobra"
)

//go:embed skill.md
var skillDoc string

var skillCmd = &cobra.Command{
	Use:   "skill",
	Short: "Print the embedded agent reference",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Fprint(stdout, skillDoc)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(skillCmd)
}
