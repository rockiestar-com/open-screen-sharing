//go:build darwin && !ios

package main

import (
	"os"
	"runtime"
	runtimeDebug "runtime/debug"

	"github.com/spf13/cobra"
)

var commandVersion = &cobra.Command{
	Use:   "version",
	Short: "Print the version",
	Args:  cobra.NoArgs,
	Run:   printVersion,
}

func init() {
	mainCommand.AddCommand(commandVersion)
}

func printVersion(cmd *cobra.Command, args []string) {
	version := "(devel)"
	var revision string
	info, loaded := runtimeDebug.ReadBuildInfo()
	if loaded {
		version = info.Main.Version
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				revision = setting.Value
			}
		}
	}
	output := "iOS-server version " + version + "\n\n"
	output += "Environment: " + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH + "\n"
	if revision != "" {
		output += "Revision: " + revision + "\n"
	}
	os.Stdout.WriteString(output)
}
