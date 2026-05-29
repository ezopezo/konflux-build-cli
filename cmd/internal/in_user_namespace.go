package internal

import (
	"github.com/spf13/cobra"

	"github.com/konflux-ci/konflux-build-cli/pkg/commands"
	l "github.com/konflux-ci/konflux-build-cli/pkg/logger"
)

var InUserNamespaceCmd = &cobra.Command{
	Use:   "in-user-namespace [flags] command [args...]",
	Short: "Run a command inside an externally created user namespace",
	Long: `Run a command inside an externally created user namespace
(e.g. by unshare or 'buildah unshare').

Flags must come before the command. Everything after the first
non-flag argument (or after --) is passed to the command as-is.`,
	Example: `  buildah unshare -- unshare --net -- konflux-build-cli internal in-user-namespace --loopback-up -- buildah build .`,
	Args:    cobra.ArbitraryArgs,
	Run: func(cmd *cobra.Command, args []string) {
		loopbackUp, _ := cmd.Flags().GetBool("loopback-up")
		if err := commands.RunInUserNamespace(loopbackUp, args); err != nil {
			l.Logger.Fatal(err)
		}
	},
}

func init() {
	InUserNamespaceCmd.Flags().SetInterspersed(false)
	InUserNamespaceCmd.Flags().Bool("loopback-up", false, "Bring up the loopback interface before executing the command")
}
