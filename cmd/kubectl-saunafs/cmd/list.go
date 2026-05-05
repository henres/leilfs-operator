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
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var saunafsClusterGVR = schema.GroupVersionResource{
	Group:    "leilfs.leilfs-operator.io",
	Version:  "v1alpha1",
	Resource: "leilfsclusters",
}

func newListCmd(opts *rootOptions) *cobra.Command {
	var allNamespaces bool

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls", "get"},
		Short:   "List LeilFSCluster resources",
		Long: `List all LeilFSCluster resources in the current or specified namespace.

Examples:
  # List clusters in the current namespace
  kubectl saunafs list

  # List clusters in a specific namespace
  kubectl saunafs list -n my-namespace

  # List clusters in all namespaces
  kubectl saunafs list -A`,
		RunE: func(c *cobra.Command, args []string) error {
			return runList(opts, allNamespaces)
		},
	}

	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false,
		"List resources across all namespaces")

	return cmd
}

func runList(opts *rootOptions, allNamespaces bool) error {
	cfg, err := buildConfig(opts.kubeconfig)
	if err != nil {
		return fmt.Errorf("building kubeconfig: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	ns := ""
	if !allNamespaces {
		ns, err = resolveNamespace(opts.kubeconfig, opts.namespace)
		if err != nil {
			return err
		}
	}

	ctx := context.Background()
	rawList, err := dynClient.Resource(saunafsClusterGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing LeilFSClusters: %w", err)
	}

	if len(rawList.Items) == 0 {
		if allNamespaces {
			fmt.Println("No LeilFSCluster resources found.")
		} else {
			fmt.Printf("No LeilFSCluster resources found in namespace %q.\n", ns)
		}
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()

	if allNamespaces {
		fmt.Fprintln(w, "NAMESPACE\tNAME\tREADY\tREASON\tCHUNK SERVERS\tAGE")
	} else {
		fmt.Fprintln(w, "NAME\tREADY\tREASON\tCHUNK SERVERS\tAGE")
	}

	for _, item := range rawList.Items {
		itemNamespace := item.GetNamespace()
		name := item.GetName()
		creationTime := item.GetCreationTimestamp()
		age := formatAge(creationTime.Time)

		ready := extractConditionStatus(item.Object, "Ready")
		reason := extractConditionReason(item.Object, "Ready")
		readyChunks := extractInt64(item.Object, "status", "readyChunkServers")
		totalChunks := extractInt64(item.Object, "status", "totalChunkServers")
		chunkStr := fmt.Sprintf("%d/%d", readyChunks, totalChunks)

		if allNamespaces {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				itemNamespace, name, ready, reason, chunkStr, age)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				name, ready, reason, chunkStr, age)
		}
	}

	return nil
}

// formatAge returns a human-readable duration since t.
func formatAge(t time.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// extractConditionStatus walks the unstructured object and returns the status of the named condition.
func extractConditionStatus(obj map[string]interface{}, condType string) string {
	conditions := extractConditions(obj)
	for _, c := range conditions {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if m["type"] == condType {
			if s, ok := m["status"].(string); ok {
				return s
			}
		}
	}
	return "Unknown"
}

// extractConditionReason returns the reason field of the named condition.
func extractConditionReason(obj map[string]interface{}, condType string) string {
	conditions := extractConditions(obj)
	for _, c := range conditions {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if m["type"] == condType {
			if r, ok := m["reason"].(string); ok {
				return r
			}
		}
	}
	return ""
}

func extractConditions(obj map[string]interface{}) []interface{} {
	status, ok := obj["status"].(map[string]interface{})
	if !ok {
		return nil
	}
	conds, _ := status["conditions"].([]interface{})
	return conds
}

// extractInt64 safely walks nested map keys and returns int64.
func extractInt64(obj map[string]interface{}, keys ...string) int64 {
	cur := interface{}(obj)
	for _, k := range keys {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return 0
		}
		cur = m[k]
	}
	switch v := cur.(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case int32:
		return int64(v)
	}
	return 0
}

// extractString safely walks nested map keys and returns a string.
func extractString(obj map[string]interface{}, keys ...string) string {
	cur := interface{}(obj)
	for _, k := range keys {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return ""
		}
		cur = m[k]
	}
	s, _ := cur.(string)
	return s
}
