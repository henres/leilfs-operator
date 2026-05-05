/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmd

import (
	"github.com/spf13/cobra"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"path/filepath"
)

// rootOptions holds flags shared by every sub-command.
type rootOptions struct {
	kubeconfig    string
	namespace     string
	allNamespaces bool
}

// NewRootCmd builds and returns the root cobra command for the plugin.
func NewRootCmd() *cobra.Command {
	opts := &rootOptions{}

	root := &cobra.Command{
		Use:   "kubectl-leilfs",
		Short: "Manage LeilFS clusters deployed by the LeilFS operator",
		Long: `kubectl-leilfs is a kubectl plugin that lets you inspect and operate
LeilFSCluster resources managed by the saunafs-operator.

Examples:
  # List all LeilFSClusters in the current namespace
  kubectl leilfs list

  # Show detailed status of a cluster
  kubectl leilfs status my-cluster

  # Show the master/chunkserver topology
  kubectl leilfs topology my-cluster

  # Show the configured storage goals
  kubectl leilfs goals my-cluster

  # Stream logs from the master pod
  kubectl leilfs logs my-cluster

	# Run a saunafs-admin command on the master pod
  kubectl leilfs admin my-cluster -- info

  # Show the goal of a path
  kubectl leilfs filegoal get my-cluster /

  # Change the goal of a directory recursively
  kubectl leilfs filegoal set my-cluster ec_4_2 /data -r`,
		SilenceUsage: true,
	}

	// Persistent flags available to every sub-command.
	home, _ := os.UserHomeDir()
	defaultKubeconfig := filepath.Join(home, ".kube", "config")
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		defaultKubeconfig = kc
	}
	root.PersistentFlags().StringVar(&opts.kubeconfig, "kubeconfig", defaultKubeconfig,
		"Path to the kubeconfig file")
	root.PersistentFlags().StringVarP(&opts.namespace, "namespace", "n", "",
		"Kubernetes namespace (defaults to current context namespace)")

	// Register sub-commands.
	root.AddCommand(
		newListCmd(opts),
		newStatusCmd(opts),
		newTopologyCmd(opts),
		newGoalsCmd(opts),
		newLogsCmd(opts),
		newAdminCmd(opts),
		newFileGoalCmd(opts),
	)

	return root
}

// buildConfig creates a *rest.Config from the kubeconfig path.
func buildConfig(kubeconfig string) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	configOverrides := &clientcmd.ConfigOverrides{}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, configOverrides,
	).ClientConfig()
}

// resolveNamespace returns the namespace to use: flag > context default.
func resolveNamespace(kubeconfig, flagNamespace string) (string, error) {
	if flagNamespace != "" {
		return flagNamespace, nil
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	ns, _, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, &clientcmd.ConfigOverrides{},
	).Namespace()
	if err != nil {
		return "default", nil //nolint:nilerr
	}
	if ns == "" {
		return "default", nil
	}
	return ns, nil
}
