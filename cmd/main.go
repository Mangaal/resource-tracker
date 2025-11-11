package main

import (
	"os"
	"time"

	"github.com/anandf/resource-tracker/cmd/tracker"
	"github.com/anandf/resource-tracker/pkg/argocd"
	"github.com/spf13/cobra"
)

// ResourceTrackerConfig contains global configuration and required runtime data
type ResourceTrackerConfig struct {
	ArgocdNamespace          string
	CheckInterval            time.Duration
	ArgoClient               argocd.ArgoCD
	LogLevel                 string
	RepoServerAddress        string
	RepoServerPlaintext      bool
	RepoServerStrictTLS      bool
	RepoServerTimeoutSeconds int
}

// newRootCommand implements the root command of argocd-resource-tracker
func newRootCommand() error {
	var rootCmd = &cobra.Command{
		Use:   "argocd-resource-tracker",
		Short: "ArgoCD Resource Tracker Plugin - Analyze resource relationships and dependencies",
		Long:  "ArgoCD Resource Tracker Plugin provides deep analysis of resource relationships and dependencies for ArgoCD applications using graph-based analysis",
	}
	rootCmd.AddCommand(newRunCommand())
	rootCmd.AddCommand(newRunQueryCommand())
	rootCmd.AddCommand(newVersionCommand())
	rootCmd.AddCommand(tracker.NewResourceTrackerCommand())
	err := rootCmd.Execute()
	return err
}

func main() {
	err := newRootCommand()
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
