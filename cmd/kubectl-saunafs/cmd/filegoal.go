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
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

func newFileGoalCmd(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "filegoal <cluster-name>",
		Short: "Get or set the storage goal of a file or directory",
		Long: `Get or set the SaunaFS storage goal on a file or directory inside the cluster.

This command operates on paths inside the SaunaFS filesystem (not host paths).
It spins up a short-lived Pod using the saunafs-client image, mounts the
filesystem, runs saunafs getgoal / saunafs setgoal, then removes the pod.

Use the 'get' subcommand to read the current goal, and 'set' to change it.`,
	}

	cmd.AddCommand(
		newFileGoalGetCmd(opts),
		newFileGoalSetCmd(opts),
	)

	return cmd
}

// ── filegoal get ─────────────────────────────────────────────────────────────

func newFileGoalGetCmd(opts *rootOptions) *cobra.Command {
	var recursive bool
	var clientImage string

	cmd := &cobra.Command{
		Use:   "get <cluster-name> <path>",
		Short: "Show the storage goal of a file or directory",
		Long: `Display the current SaunaFS storage goal for a path inside the filesystem.

The path must be a SaunaFS path (e.g. / or /mydir/myfile), not a host path.
A short-lived client Pod is created to run 'saunafs getgoal', then deleted.

Examples:
  # Show the goal of the root directory
  kubectl saunafs filegoal get my-cluster /

  # Show goals recursively for a directory
  kubectl saunafs filegoal get my-cluster /data -r`,
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			return runFileGoalGet(opts, args[0], args[1], recursive, clientImage)
		},
	}

	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false,
		"Recurse into subdirectories")
	cmd.Flags().StringVar(&clientImage, "client-image", "",
		"Override the saunafs-client image (default: value from spec.csi.image or saunafs-client:latest)")

	return cmd
}

func runFileGoalGet(opts *rootOptions, clusterName, path string, recursive bool, clientImageOverride string) error {
	k8s, ns, clientImage, masterSvcName, err := prepareFileGoalRun(opts, clusterName, clientImageOverride)
	if err != nil {
		return err
	}

	// saunafs getgoal [-r] <path> <master-host> <master-port>
	args := []string{"saunafs", "getgoal"}
	if recursive {
		args = append(args, "-r")
	}
	args = append(args, path, masterSvcName, "9421")

	fmt.Fprintf(os.Stderr, "Using client image: %s\n", clientImage)
	fmt.Fprintf(os.Stderr, "Running: %v\n\n", args)

	return runEphemeralPod(context.Background(), k8s, ns, clusterName, clientImage, args)
}

// ── filegoal set ─────────────────────────────────────────────────────────────

func newFileGoalSetCmd(opts *rootOptions) *cobra.Command {
	var recursive bool
	var clientImage string

	cmd := &cobra.Command{
		Use:   "set <cluster-name> <goal> <path>",
		Short: "Change the storage goal of a file or directory",
		Long: `Set the SaunaFS storage goal for a path inside the filesystem.

The goal can be a name (e.g. ec_4_2) or a numeric ID (e.g. 10).
The path must be a SaunaFS path (e.g. / or /mydir), not a host path.
A short-lived client Pod is created to run 'saunafs setgoal', then deleted.

The metadata change is immediate; physical chunk rebalancing is asynchronous
and may take minutes to hours depending on the amount of data.

Examples:
  # Set goal ec_4_2 on a single file
  kubectl saunafs filegoal set my-cluster ec_4_2 /myfile

  # Set goal recursively on a directory
  kubectl saunafs filegoal set my-cluster ec_4_2 /data -r

  # Set goal by numeric ID
  kubectl saunafs filegoal set my-cluster 2 /data -r`,
		Args: cobra.ExactArgs(3),
		RunE: func(c *cobra.Command, args []string) error {
			return runFileGoalSet(opts, args[0], args[1], args[2], recursive, clientImage)
		},
	}

	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false,
		"Recurse into subdirectories")
	cmd.Flags().StringVar(&clientImage, "client-image", "",
		"Override the saunafs-client image (default: value from spec.csi.image or saunafs-client:latest)")

	return cmd
}

func runFileGoalSet(opts *rootOptions, clusterName, goal, path string, recursive bool, clientImageOverride string) error {
	k8s, ns, clientImage, masterSvcName, err := prepareFileGoalRun(opts, clusterName, clientImageOverride)
	if err != nil {
		return err
	}

	// saunafs setgoal [-r] <goal> <path> <master-host> <master-port>
	args := []string{"saunafs", "setgoal"}
	if recursive {
		args = append(args, "-r")
	}
	args = append(args, goal, path, masterSvcName, "9421")

	fmt.Fprintf(os.Stderr, "Using client image: %s\n", clientImage)
	fmt.Fprintf(os.Stderr, "Running: %v\n\n", args)

	return runEphemeralPod(context.Background(), k8s, ns, clusterName, clientImage, args)
}

// ── shared setup ─────────────────────────────────────────────────────────────

// prepareFileGoalRun resolves the Kubernetes clients, namespace, client image,
// and master service name needed by both get and set subcommands.
func prepareFileGoalRun(opts *rootOptions, clusterName, clientImageOverride string) (
	k8sClient kubernetes.Interface,
	ns string,
	clientImage string,
	masterSvcName string,
	err error,
) {
	cfg, err := buildConfig(opts.kubeconfig)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("building kubeconfig: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("creating dynamic client: %w", err)
	}

	k8sClient, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("creating kubernetes client: %w", err)
	}

	ns, err = resolveNamespace(opts.kubeconfig, opts.namespace)
	if err != nil {
		return nil, "", "", "", err
	}

	ctx := context.Background()
	clusterObj, err := dynClient.Resource(saunafsClusterGVR).Namespace(ns).Get(ctx, clusterName, metav1.GetOptions{})
	if err != nil {
		return nil, "", "", "", fmt.Errorf("SaunaFSCluster %q not found in namespace %q: %w", clusterName, ns, err)
	}

	clientImage = clientImageOverride
	if clientImage == "" {
		clientImage = extractString(clusterObj.Object, "spec", "csi", "image")
	}
	if clientImage == "" {
		clientImage = "saunafs-client:latest"
	}

	masterSvcName = fmt.Sprintf("%s-master", clusterName)

	return k8sClient, ns, clientImage, masterSvcName, nil
}
