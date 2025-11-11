package tracker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	argocdcs "github.com/argoproj/argo-cd/v3/pkg/client/clientset/versioned"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// NewResourceTrackerCommand creates a new resource tracker command
func NewResourceTrackerCommand() *cobra.Command {
	var (
		appName        string
		appNamespace   string
		namespace      string
		kubeconfig     string
		allApps        bool
		relationSource string
	)

	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Analyze resource relationships and dependencies for ArgoCD applications",
		Long:  "Analyze resource relationships and dependencies for ArgoCD applications. Can process a single app or all apps.",
		RunE: func(cmd *cobra.Command, args []string) error {
			var apps []*v1alpha1.Application
			if !allApps && appName == "" {
				return fmt.Errorf("either --app or --all-apps must be specified")
			}

			// Build kube rest config from kubeconfig
			if kubeconfig == "" {
				homeDir, err := os.UserHomeDir()
				if err != nil {
					return fmt.Errorf("failed to get home directory: %w", err)
				}
				kubeconfig = filepath.Join(homeDir, ".kube", "config")
			}

			config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
			if err != nil {
				return fmt.Errorf("failed to load kubeconfig: %w", err)
			}

			// Kubernetes-based Argo CD Application clientset
			argoClient, err := argocdcs.NewForConfig(config)
			if err != nil {
				return fmt.Errorf("failed to create Argo CD application client: %w", err)
			}

			clientset, err := kubernetes.NewForConfig(config)
			if err != nil {
				return err
			}

			// Initialize tracker with Kubernetes client
			tracker := NewResourceTracker(argoClient, clientset, namespace)
			tracker.kubeconfig = kubeconfig
			if allApps {
				appsList, err := argoClient.ArgoprojV1alpha1().Applications(appNamespace).List(context.Background(), metav1.ListOptions{})
				if err != nil {
					return fmt.Errorf("failed to list applications: %w", err)
				}
				apps = make([]*v1alpha1.Application, 0, len(appsList.Items))
				for i := range appsList.Items {
					apps = append(apps, &appsList.Items[i])
				}
			} else {
				app, err := argoClient.ArgoprojV1alpha1().Applications(appNamespace).Get(context.Background(), appName, metav1.GetOptions{})
				if err != nil {
					return fmt.Errorf("failed to get application: %w", err)
				}
				apps = []*v1alpha1.Application{app}
			}

			fmt.Printf("analyze: relationSource=%s apps=%d appNamespace=%s\n", relationSource, len(apps), appNamespace)

			// Relationship source selection
			switch relationSource {
			case "resourcegraph":
				return tracker.AnalyzeWithResourceGraph(config, apps)
			case "graph":
				return tracker.AnalyzeWithGraph(config, apps)
			default:
				return fmt.Errorf("invalid --relation-source: %s (use 'resourcegraph' or 'graph')", relationSource)
			}
		},
	}

	// Add flags
	cmd.Flags().StringVarP(&appName, "app", "a", "", "Application name (required for single app analysis)")
	cmd.Flags().StringVarP(&appNamespace, "appNamespace", "", "argocd", "Application namespace")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "argocd", "ArgoCD namespace")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file for cluster access")
	cmd.Flags().BoolVar(&allApps, "all-apps", false, "Analyze all applications in the namespace")
	cmd.Flags().StringVar(&relationSource, "relation-source", "resourcegraph", "Relationship backend: 'resourcegraph' or 'graph'")

	return cmd
}
