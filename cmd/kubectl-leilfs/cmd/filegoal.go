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
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// mountPoint is where the LeilFS root is mounted inside the ephemeral pod.
const mountPoint = "/mnt/saunafs"

func newFileGoalCmd(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "filegoal <cluster-name>",
		Short: "Get or set the storage goal of a file or directory",
		Long: `Get or set the LeilFS storage goal on a file or directory inside the cluster.

saunafs getgoal / setgoal operate on paths inside a mounted LeilFS filesystem.
This command mounts the filesystem via sfsmount inside a short-lived privileged
Pod, runs the tool, then unmounts and removes the pod.

The <path> argument is relative to the root of the LeilFS filesystem
(e.g. / or /mydir/myfile).`,
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
		Long: `Display the current LeilFS storage goal for a path inside the filesystem.

The path is relative to the LeilFS root (e.g. / or /mydir/myfile).
A privileged client Pod is created to mount the filesystem via sfsmount and run
'saunafs getgoal', then deleted.

Examples:
  # Show the goal of the root directory
  kubectl leilfs filegoal get my-cluster /

  # Show goals recursively for a directory
  kubectl leilfs filegoal get my-cluster /data -r`,
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			return runFileGoalGet(opts, args[0], args[1], recursive, clientImage)
		},
	}

	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false,
		"Recurse into subdirectories")
	cmd.Flags().StringVar(&clientImage, "client-image", "",
		"Override the leilfs-client image (default: value from spec.csi.image or leilfs-client:latest)")

	return cmd
}

func runFileGoalGet(opts *rootOptions, clusterName, path string, recursive bool, clientImageOverride string) error {
	k8s, ns, clientImage, masterSvcName, err := prepareFileGoalRun(opts, clusterName, clientImageOverride)
	if err != nil {
		return err
	}

	// saunafs getgoal [-r] <mountpoint/path>
	getgoalCmd := "saunafs getgoal"
	if recursive {
		getgoalCmd += " -r"
	}
	getgoalCmd += " " + mountPoint + normalizePath(path)

	script := buildMountScript(masterSvcName, getgoalCmd)

	fmt.Fprintf(os.Stderr, "Using client image: %s\n", clientImage)
	fmt.Fprintf(os.Stderr, "Mounting LeilFS from %s, then: saunafs getgoal %s\n\n", masterSvcName, path)

	return runFUSEPod(context.Background(), k8s, ns, clusterName, clientImage, script)
}

// ── filegoal set ─────────────────────────────────────────────────────────────

func newFileGoalSetCmd(opts *rootOptions) *cobra.Command {
	var recursive bool
	var clientImage string

	cmd := &cobra.Command{
		Use:   "set <cluster-name> <goal> <path>",
		Short: "Change the storage goal of a file or directory",
		Long: `Set the LeilFS storage goal for a path inside the filesystem.

The goal can be a name (e.g. ec_4_2) or a numeric ID (e.g. 10).
The path is relative to the LeilFS root (e.g. / or /mydir).
A privileged client Pod is created to mount the filesystem via sfsmount and run
'saunafs setgoal', then deleted.

The metadata change is immediate; physical chunk rebalancing is asynchronous
and may take minutes to hours depending on the amount of data.

Examples:
  # Set goal ec_4_2 on a single file
  kubectl leilfs filegoal set my-cluster ec_4_2 /myfile

  # Set goal recursively on a directory
  kubectl leilfs filegoal set my-cluster ec_4_2 /data -r

  # Apply to the entire filesystem
  kubectl leilfs filegoal set my-cluster ec_4_2 / -r`,
		Args: cobra.ExactArgs(3),
		RunE: func(c *cobra.Command, args []string) error {
			return runFileGoalSet(opts, args[0], args[1], args[2], recursive, clientImage)
		},
	}

	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false,
		"Recurse into subdirectories")
	cmd.Flags().StringVar(&clientImage, "client-image", "",
		"Override the leilfs-client image (default: value from spec.csi.image or leilfs-client:latest)")

	return cmd
}

func runFileGoalSet(opts *rootOptions, clusterName, goal, path string, recursive bool, clientImageOverride string) error {
	k8s, ns, clientImage, masterSvcName, err := prepareFileGoalRun(opts, clusterName, clientImageOverride)
	if err != nil {
		return err
	}

	// saunafs setgoal [-r] <goal> <mountpoint/path>
	setgoalCmd := fmt.Sprintf("saunafs setgoal %s", goal)
	if recursive {
		setgoalCmd += " -r"
	}
	setgoalCmd += " " + mountPoint + normalizePath(path)

	script := buildMountScript(masterSvcName, setgoalCmd)

	fmt.Fprintf(os.Stderr, "Using client image: %s\n", clientImage)
	fmt.Fprintf(os.Stderr, "Mounting LeilFS from %s, then: saunafs setgoal %s %s\n\n", masterSvcName, goal, path)

	return runFUSEPod(context.Background(), k8s, ns, clusterName, clientImage, script)
}

// ── shared helpers ────────────────────────────────────────────────────────────

// normalizePath ensures the path starts with / and has no trailing slash
// (except for the root "/").
func normalizePath(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if p != "/" {
		p = strings.TrimRight(p, "/")
	}
	// Root stays as "" so mountPoint+normalizePath("/") == "/mnt/saunafs"
	if p == "/" {
		return ""
	}
	return p
}

// buildMountScript returns a shell script that:
//  1. Creates the mountpoint directory
//  2. Mounts the LeilFS root via sfsmount
//  3. Runs the given command
//  4. Unmounts (best effort)
func buildMountScript(masterSvcName, command string) string {
	return fmt.Sprintf(`set -e
mkdir -p %s
sfsmount %s -H %s
%s
sfsmount -u %s || true`,
		mountPoint,
		mountPoint,
		masterSvcName,
		command,
		mountPoint,
	)
}

// runFUSEPod creates a privileged pod with /dev/fuse that runs a shell script,
// streams its output, then deletes the pod.
func runFUSEPod(
	ctx context.Context,
	k8s kubernetes.Interface,
	ns, clusterName, image, script string,
) error {
	privileged := true
	hostPathType := corev1.HostPathCharDev

	podName := fmt.Sprintf("%s-filegoal-%d", clusterName, time.Now().UnixNano()%1_000_000_000)

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
					Name:            "leilfs-client",
					Image:           image,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         []string{"sh", "-c", script},
					SecurityContext: &corev1.SecurityContext{
						Privileged: &privileged,
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "fuse", MountPath: "/dev/fuse"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "fuse",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: "/dev/fuse",
							Type: &hostPathType,
						},
					},
				},
			},
		},
	}

	if _, err := k8s.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating filegoal pod: %w", err)
	}

	return streamAndWaitPod(ctx, k8s, ns, podName, "leilfs-client")
}

// prepareFileGoalRun resolves clients, namespace, client image and master
// service name — shared by both get and set subcommands.
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
		return nil, "", "", "", fmt.Errorf("LeilFSCluster %q not found in namespace %q: %w", clusterName, ns, err)
	}

	clientImage = clientImageOverride
	if clientImage == "" {
		clientImage = extractString(clusterObj.Object, "spec", "csi", "image")
	}
	if clientImage == "" {
		clientImage = "leilfs-client:latest"
	}

	masterSvcName = fmt.Sprintf("%s-master", clusterName)
	return k8sClient, ns, clientImage, masterSvcName, nil
}
