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
	"io"
	"os"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

func newLogsCmd(opts *rootOptions) *cobra.Command {
	var (
		component  string
		serverName string
		follow     bool
		tail       int64
		previous   bool
	)

	cmd := &cobra.Command{
		Use:   "logs <cluster-name>",
		Short: "Stream logs from a LeilFSCluster component",
		Long: `Fetch or stream logs from a pod belonging to a LeilFSCluster component.

By default, logs from the master pod are shown. Use --component to target a
specific component, and --server to select a specific chunk server by name.

Examples:
  # Logs from the master pod
  kubectl saunafs logs my-cluster

  # Follow (tail -f) master logs
  kubectl saunafs logs my-cluster --follow

  # Last 100 lines from the master
  kubectl saunafs logs my-cluster --tail 100

  # Logs from the NFS gateway pod
  kubectl saunafs logs my-cluster --component nfs

  # Logs from a specific chunk server
  kubectl saunafs logs my-cluster --component chunk --server cs-hdd001`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runLogs(opts, args[0], component, serverName, follow, tail, previous)
		},
	}

	cmd.Flags().StringVarP(&component, "component", "c", "master",
		"Component to get logs from: master, chunk, nfs, interface")
	cmd.Flags().StringVar(&serverName, "server", "",
		"Chunk server name (required when --component=chunk)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false,
		"Stream (follow) the logs")
	cmd.Flags().Int64Var(&tail, "tail", -1,
		"Number of lines from the end of the logs to show (-1 = all)")
	cmd.Flags().BoolVarP(&previous, "previous", "p", false,
		"Show logs from the previous (crashed) container instance")

	return cmd
}

func runLogs(opts *rootOptions, clusterName, component, serverName string, follow bool, tail int64, previous bool) error {
	cfg, err := buildConfig(opts.kubeconfig)
	if err != nil {
		return fmt.Errorf("building kubeconfig: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	k8sClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating kubernetes client: %w", err)
	}

	ns, err := resolveNamespace(opts.kubeconfig, opts.namespace)
	if err != nil {
		return err
	}

	ctx := context.Background()

	// Verify the cluster exists.
	clusterObj, err := dynClient.Resource(saunafsClusterGVR).Namespace(ns).Get(ctx, clusterName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("LeilFSCluster %q not found in namespace %q: %w", clusterName, ns, err)
	}
	_ = clusterObj

	// Resolve the target pod name.
	podName, containerName, err := resolveLogTarget(ctx, k8sClient, ns, clusterName, component, serverName)
	if err != nil {
		return err
	}

	// Build log options.
	logOpts := &corev1.PodLogOptions{
		Container: containerName,
		Follow:    follow,
		Previous:  previous,
	}
	if tail >= 0 {
		t := tail
		logOpts.TailLines = &t
	}

	fmt.Fprintf(os.Stderr, "Fetching logs from pod %q (container: %q)...\n", podName, containerName)

	req := k8sClient.CoreV1().Pods(ns).GetLogs(podName, logOpts)
	stream, err := req.Stream(ctx)
	if err != nil {
		return fmt.Errorf("opening log stream for pod %q: %w", podName, err)
	}
	defer stream.Close()

	_, err = io.Copy(os.Stdout, stream)
	return err
}

// resolveLogTarget finds the pod and container name for the given component.
func resolveLogTarget(
	ctx context.Context,
	k8s kubernetes.Interface,
	ns, clusterName, component, serverName string,
) (podName, containerName string, err error) {
	var labelSelector string

	// Labels applied by the controller (see reconcileMasterDaemonSet,
	// reconcileChunkStatefulSet, reconcileNFS, reconcileInterface):
	//   app.kubernetes.io/name     = leilfs-master | leilfs-chunkserver | leilfs-nfs | leilfs-interface
	//   app.kubernetes.io/instance = <cluster-name>
	//   leilfs.io/chunk-server    = <srv-name>   (chunk pods only)
	switch component {
	case "master":
		labelSelector = fmt.Sprintf(
			"app.kubernetes.io/name=leilfs-master,app.kubernetes.io/instance=%s", clusterName)
		containerName = "leilfs-master"
	case "chunk":
		if serverName == "" {
			return "", "", fmt.Errorf("--server is required when --component=chunk")
		}
		labelSelector = fmt.Sprintf(
			"app.kubernetes.io/name=leilfs-chunkserver,app.kubernetes.io/instance=%s,leilfs.io/chunk-server=%s",
			clusterName, serverName)
		containerName = "leilfs-chunkserver"
	case "nfs":
		labelSelector = fmt.Sprintf(
			"app.kubernetes.io/name=leilfs-nfs,app.kubernetes.io/instance=%s", clusterName)
		containerName = "nfs-ganesha"
	case "interface", "webui":
		labelSelector = fmt.Sprintf(
			"app.kubernetes.io/name=leilfs-interface,app.kubernetes.io/instance=%s", clusterName)
		containerName = "leilfs-cgiserver"
	default:
		return "", "", fmt.Errorf("unknown component %q; valid values: master, chunk, nfs, interface", component)
	}

	pods, listErr := k8s.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if listErr != nil {
		return "", "", fmt.Errorf("listing pods with selector %q: %w", labelSelector, listErr)
	}

	if len(pods.Items) == 0 {
		return "", "", fmt.Errorf(
			"no pods found for component %q of cluster %q (selector: %q)",
			component, clusterName, labelSelector)
	}

	// Prefer Running pods; fall back to first in list.
	chosen := pods.Items[0]
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			chosen = p
			break
		}
	}

	return chosen.Name, containerName, nil
}
