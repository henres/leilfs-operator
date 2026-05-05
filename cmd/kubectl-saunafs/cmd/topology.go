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
	"text/tabwriter"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

func newTopologyCmd(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "topology <cluster-name>",
		Short: "Show the master/chunkserver topology of a LeilFSCluster",
		Long: `Display the node placement of each component in a LeilFSCluster:
master DaemonSet node selector, and per-chunkserver node/disk assignments.
Also shows the live pod status for each component.

Examples:
  kubectl saunafs topology my-cluster
  kubectl saunafs topology my-cluster -n my-namespace`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runTopology(opts, args[0])
		},
	}
}

func runTopology(opts *rootOptions, name string) error {
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

	obj, err := dynClient.Resource(saunafsClusterGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting LeilFSCluster %q in namespace %q: %w", name, ns, err)
	}
	data := obj.Object

	fmt.Printf("Topology for LeilFSCluster %q (namespace: %s)\n\n", name, ns)

	// --- Master ---
	fmt.Println("Master:")
	masterSelector := extractMap(data, "spec", "master", "nodeSelector")
	masterImage := extractString(data, "spec", "master", "image")
	if masterImage == "" {
		masterImage = "<default>"
	}
	fmt.Printf("  Image:         %s\n", masterImage)
	if len(masterSelector) > 0 {
		fmt.Printf("  Node selector: %s\n", renderLabels(masterSelector))
	} else {
		fmt.Println("  Node selector: <none>")
	}

	// Master pods: label app.kubernetes.io/name=leilfs-master + instance=<cluster>
	masterPods, err := k8sClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf(
			"app.kubernetes.io/name=leilfs-master,app.kubernetes.io/instance=%s", name),
	})
	if err == nil && len(masterPods.Items) > 0 {
		fmt.Println("  Pods:")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "    NAME\tNODE\tSTATUS\tRESTARTS")
		for _, p := range masterPods.Items {
			restarts := int32(0)
			for _, cs := range p.Status.ContainerStatuses {
				restarts += cs.RestartCount
			}
			fmt.Fprintf(w, "    %s\t%s\t%s\t%d\n",
				p.Name, p.Spec.NodeName, string(p.Status.Phase), restarts)
		}
		w.Flush()
	} else {
		fmt.Println("  Pods:  <none>")
	}
	fmt.Println()

	// --- Chunk servers ---
	servers := extractSlice(data, "spec", "chunk", "servers")
	fmt.Printf("Chunk Servers (%d):\n", len(servers))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  NAME\tNODE\tMOUNTS\tPOD STATUS")

	for _, s := range servers {
		sm, ok := s.(map[string]interface{})
		if !ok {
			continue
		}
		sName := stringVal(sm["name"])
		sNode := stringVal(sm["nodeName"])
		mounts := extractSliceRaw(sm, "mountPaths")

		// Build mount summary.
		mountSummary := ""
		for i, mp := range mounts {
			mpm, ok := mp.(map[string]interface{})
			if !ok {
				continue
			}
			path := stringVal(mpm["path"])
			hostPath := stringVal(mpm["hostPath"])
			claimName := stringVal(mpm["claimName"])
			storageClass := stringVal(mpm["storageClassName"])
			var src string
			switch {
			case hostPath != "":
				src = "host:" + hostPath
			case claimName != "":
				src = "pvc:" + claimName
			case storageClass != "":
				src = "dynamic:" + storageClass
			default:
				src = "?"
			}
			if i == 0 {
				mountSummary = fmt.Sprintf("%s→%s", path, src)
			} else {
				mountSummary += fmt.Sprintf(", %s→%s", path, src)
			}
		}
		if mountSummary == "" {
			mountSummary = "<none>"
		}

		// Query pod status via the labels applied by the controller:
		//   app.kubernetes.io/name=leilfs-chunkserver
		//   app.kubernetes.io/instance=<cluster>
		//   leilfs.io/chunk-server=<srv-name>
		podStatus := "<unknown>"
		pods, podErr := k8sClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf(
				"app.kubernetes.io/name=leilfs-chunkserver,app.kubernetes.io/instance=%s,leilfs.io/chunk-server=%s",
				name, sName),
		})
		if podErr == nil && len(pods.Items) > 0 {
			podStatus = string(pods.Items[0].Status.Phase)
		}

		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", sName, sNode, mountSummary, podStatus)
	}
	w.Flush()

	fmt.Println()

	// --- Optional services ---
	nfsEnabled := extractBool(data, "spec", "nfs", "enabled")
	webUIEnabled := extractBool(data, "spec", "interface", "enabled")
	exposeEnabled := extractBool(data, "spec", "expose", "enabled")

	if nfsEnabled || webUIEnabled || exposeEnabled {
		fmt.Println("Optional Services:")
		if nfsEnabled {
			nfsNP := extractInt64(data, "spec", "nfs", "nodePort")
			fmt.Printf("  NFS Gateway:  enabled  (NodePort: %d)\n", nfsNP)
		}
		if webUIEnabled {
			webuiPort := extractInt64(data, "spec", "interface", "port")
			fmt.Printf("  Web UI:       enabled  (port: %d)\n", webuiPort)
		}
		if exposeEnabled {
			clientNP := extractInt64(data, "spec", "expose", "clientNodePort")
			fmt.Printf("  Expose:       enabled  (client NodePort: %d)\n", clientNP)
		}
	}

	return nil
}
