package version

import (
	"github.com/spf13/cobra"

	"github.com/teren-papercutlabs/pclaw/cmd/pclaw/internal"
	"github.com/teren-papercutlabs/pclaw/cmd/pclaw/internal/cliui"
	"github.com/teren-papercutlabs/pclaw/pkg/config"
)

func NewVersionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "version",
		Aliases: []string{"v"},
		Short:   "Show version information",
		Run: func(_ *cobra.Command, _ []string) {
			printVersion()
		},
	}

	return cmd
}

func printVersion() {
	build, goVer := config.FormatBuildInfo()
	cliui.PrintVersion(internal.Logo, "picoclaw "+config.FormatVersion(), build, goVer)
}
