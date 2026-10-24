//go:build darwin && !ios

package main

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

var (
	debugLog bool
	logLevel slog.LevelVar
	logger   = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: &logLevel}))
)

var mainCommand = &cobra.Command{
	Use:              "iOS-server",
	Short:            "Share the screen of an iPhone or iPad connected to this Mac",
	PersistentPreRun: preRun,
	SilenceErrors:    true,
}

func init() {
	mainCommand.PersistentFlags().BoolVar(&debugLog, "debug", false, "print debug logs")
}

func preRun(cmd *cobra.Command, args []string) {
	if debugLog {
		logLevel.Set(slog.LevelDebug)
	}
}

func fatal(err error) {
	logger.Error(err.Error())
	os.Exit(1)
}
