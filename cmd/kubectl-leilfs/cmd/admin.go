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
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

func newAdminCmd(opts *rootOptions) *cobra.Command {
	var clientImage string

	cmd := &cobra.Command{
		Use:   "admin <cluster-name> -- <saunafs-admin-args...>",
		Short: "Execute saunafs-admin commands against the master",
		Long: `Run a saunafs-admin command against a LeilFSCluster master.

Because saunafs-admin is part of the saunafs-client package (not installed in
the master image), this command spins up a short-lived Pod using the client
image, runs saunafs-admin there, streams the output, then deletes the Pod.

The client image is read from spec.csi.image (usually leilfs-client:latest).
Override it with --client-image if needed.

The master service hostname and port are resolved automatically from the
LeilFSCluster spec (saunafs-admin uses the client port, 9421 by default).

Examples:
  # Show cluster information
  kubectl leilfs admin my-cluster -- info

  # List registered chunk servers
  kubectl leilfs admin my-cluster -- list-chunkservers

  # List chunk server disks (verbose)
  kubectl leilfs admin my-cluster -- list-disks --verbose

  # Show storage goals usage
  kubectl leilfs admin my-cluster -- list-goals --pretty

  # Show metadata server status
  kubectl leilfs admin my-cluster -- metadataserver-status

  # Check chunks health
  kubectl leilfs admin my-cluster -- chunks-health --availability

The subcommand and its options come after '--'; host and port are injected
automatically from the LeilFSCluster spec.`,
		DisableFlagParsing: false,
		RunE: func(c *cobra.Command, args []string) error {
			return runAdmin(opts, args, clientImage)
		},
	}

	cmd.Flags().StringVar(&clientImage, "client-image", "",
		"Override the leilfs-client image used for the ephemeral pod (default: value from spec.csi.image or leilfs-client:latest)")

	return cmd
}

func runAdmin(opts *rootOptions, args []string, clientImageOverride string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: kubectl leilfs admin <cluster-name> -- <saunafs-admin-args>")
	}
	clusterName := args[0]
	adminArgs := args[1:]

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

	// Verify the cluster exists and read its spec.
	clusterObj, err := dynClient.Resource(saunafsClusterGVR).Namespace(ns).Get(ctx, clusterName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("LeilFSCluster %q not found in namespace %q: %w", clusterName, ns, err)
	}

	// Determine the client image to use.
	clientImage := clientImageOverride
	if clientImage == "" {
		// Try spec.csi.image first.
		clientImage = extractString(clusterObj.Object, "spec", "csi", "image")
	}
	if clientImage == "" {
		clientImage = "leilfs-client:latest"
	}

	// Determine master service hostname and client port.
	// saunafs-admin communicates with the master via the client port (9421),
	// not the metalogger port (9419). We look for a port named "client" in
	// the spec first, then fall back to 9421.
	masterSvcName := fmt.Sprintf("%s-master", clusterName)
	adminPort := int64(9421)

	masterPorts := extractSlice(clusterObj.Object, "spec", "master", "ports")
	for _, p := range masterPorts {
		pm, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		if stringVal(pm["name"]) == "client" {
			if cp := extractInt64(pm, "containerPort"); cp != 0 {
				adminPort = cp
			}
		}
	}

	// Build the full saunafs-admin command.
	// Argument order: saunafs-admin COMMAND [OPTIONS] <master-ip> <port>
	// The host and port are appended after the user-supplied subcommand and
	// its options (i.e. adminArgs already contains the subcommand first).
	command := append([]string{"leilfs-admin"}, adminArgs...)
	command = append(command, masterSvcName, fmt.Sprintf("%d", adminPort))

	fmt.Fprintf(os.Stderr, "Using client image: %s\n", clientImage)
	fmt.Fprintf(os.Stderr, "Running: %v\n\n", command)

	return runEphemeralPod(ctx, k8sClient, ns, clusterName, clientImage, command)
}

// runEphemeralPod creates a short-lived Pod that runs command, streams its
// logs to stdout/stderr, and deletes the pod when done.
func runEphemeralPod(
	ctx context.Context,
	k8s kubernetes.Interface,
	ns, clusterName, image string,
	command []string,
) error {
	podName := fmt.Sprintf("%s-admin-%d", clusterName, time.Now().UnixNano()%1_000_000_000)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kubectl-leilfs",
				"app.kubernetes.io/instance":   clusterName,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:            "leilfs-admin",
					Image:           image,
					Command:         command,
					ImagePullPolicy: corev1.PullIfNotPresent,
				},
			},
		},
	}

	createdPod, err := k8s.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("creating ephemeral pod: %w", err)
	}
	return streamAndWaitPod(ctx, k8s, ns, createdPod.Name, "leilfs-admin")
}

// streamAndWaitPod waits for a pod to start, streams its logs to stdout, then
// deletes the pod. It is shared by runEphemeralPod and runPrivilegedEphemeralPod.
func streamAndWaitPod(ctx context.Context, k8s kubernetes.Interface, ns, podName, containerName string) error {
	// Ensure the pod is deleted regardless of outcome.
	defer func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = k8s.CoreV1().Pods(ns).Delete(delCtx, podName, metav1.DeleteOptions{})
	}()

	fmt.Fprintf(os.Stderr, "Waiting for pod %q to start...\n", podName)

	// Wait for the pod to reach Running or a terminal state.
	if err := waitForPodRunningOrDone(ctx, k8s, ns, podName, 60*time.Second); err != nil {
		return err
	}

	// Stream logs.
	logOpts := &corev1.PodLogOptions{
		Container: containerName,
		Follow:    true,
	}
	req := k8s.CoreV1().Pods(ns).GetLogs(podName, logOpts)
	stream, err := req.Stream(ctx)
	if err != nil {
		return fmt.Errorf("opening log stream: %w", err)
	}
	defer stream.Close()

	if _, err := io.Copy(os.Stdout, stream); err != nil && err != io.EOF {
		return fmt.Errorf("streaming logs: %w", err)
	}

	// Check final pod phase to propagate failure.
	finalPod, err := k8s.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		// Pod may already be gone; treat as success since we got the logs.
		return nil
	}
	if finalPod.Status.Phase == corev1.PodFailed {
		// Surface the container's exit message if available.
		for _, cs := range finalPod.Status.ContainerStatuses {
			if cs.Name == containerName && cs.State.Terminated != nil {
				return fmt.Errorf("pod exited with code %d: %s",
					cs.State.Terminated.ExitCode,
					cs.State.Terminated.Message)
			}
		}
		return fmt.Errorf("ephemeral pod %q failed", podName)
	}

	return nil
}

// waitForPodRunningOrDone polls until the pod leaves Pending state or the
// timeout elapses.
func waitForPodRunningOrDone(ctx context.Context, k8s kubernetes.Interface, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	pollInterval := 500 * time.Millisecond

	return wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		p, err := k8s.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		switch p.Status.Phase {
		case corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed:
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("pod %q did not start within %s (phase: %s)", name, timeout, p.Status.Phase)
		}
		return false, nil
	})
}
