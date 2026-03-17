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
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
)

func newStatusCmd(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "status <cluster-name>",
		Short: "Show the status of a SaunaFSCluster",
		Long: `Show detailed status information for a SaunaFSCluster resource.

Displays conditions, chunk server counts, master configuration, and component
enablement (NFS, WebUI, Expose).

Examples:
  kubectl saunafs status my-cluster
  kubectl saunafs status my-cluster -n my-namespace`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runStatus(opts, args[0])
		},
	}
}

func runStatus(opts *rootOptions, name string) error {
	cfg, err := buildConfig(opts.kubeconfig)
	if err != nil {
		return fmt.Errorf("building kubeconfig: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	ns, err := resolveNamespace(opts.kubeconfig, opts.namespace)
	if err != nil {
		return err
	}

	ctx := context.Background()
	obj, err := dynClient.Resource(saunafsClusterGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting SaunaFSCluster %q in namespace %q: %w", name, ns, err)
	}

	data := obj.Object

	fmt.Printf("Name:       %s\n", obj.GetName())
	fmt.Printf("Namespace:  %s\n", obj.GetNamespace())
	fmt.Printf("Created:    %s\n", obj.GetCreationTimestamp().String())
	fmt.Println()

	// --- Conditions ---
	fmt.Println("Conditions:")
	conditions := extractConditions(data)
	if len(conditions) == 0 {
		fmt.Println("  <none>")
	} else {
		for _, c := range conditions {
			m, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			cType := stringVal(m["type"])
			cStatus := stringVal(m["status"])
			cReason := stringVal(m["reason"])
			cMsg := stringVal(m["message"])
			cTime := stringVal(m["lastTransitionTime"])
			fmt.Printf("  %-20s  Status=%-8s  Reason=%-20s  LastTransition=%s\n",
				cType, cStatus, cReason, cTime)
			if cMsg != "" {
				fmt.Printf("                       Message: %s\n", cMsg)
			}
		}
	}
	fmt.Println()

	// --- Chunk servers ---
	readyChunks := extractInt64(data, "status", "readyChunkServers")
	totalChunks := extractInt64(data, "status", "totalChunkServers")
	fmt.Printf("Chunk Servers:  %d ready / %d total\n", readyChunks, totalChunks)
	fmt.Println()

	// --- Spec summary ---
	fmt.Println("Spec:")

	masterImage := extractString(data, "spec", "master", "image")
	if masterImage == "" {
		masterImage = "<default>"
	}
	fmt.Printf("  Master image:    %s\n", masterImage)

	nodeSelector := extractMap(data, "spec", "master", "nodeSelector")
	if len(nodeSelector) > 0 {
		fmt.Printf("  Master selector: %s\n", renderLabels(nodeSelector))
	}

	chunkImage := extractString(data, "spec", "chunk", "image")
	if chunkImage == "" {
		chunkImage = "<default>"
	}
	fmt.Printf("  Chunk image:     %s\n", chunkImage)

	servers := extractSlice(data, "spec", "chunk", "servers")
	fmt.Printf("  Chunk servers:   %d defined\n", len(servers))
	for _, s := range servers {
		sm, ok := s.(map[string]interface{})
		if !ok {
			continue
		}
		sName := stringVal(sm["name"])
		sNode := stringVal(sm["nodeName"])
		mounts := extractSliceRaw(sm, "mountPaths")
		fmt.Printf("    - %-20s  node=%-20s  mounts=%d\n", sName, sNode, len(mounts))
	}
	fmt.Println()

	// --- Optional components ---
	fmt.Println("Optional Components:")
	nfsEnabled := extractBool(data, "spec", "nfs", "enabled")
	webUIEnabled := extractBool(data, "spec", "interface", "enabled")
	exposeEnabled := extractBool(data, "spec", "expose", "enabled")
	csiEnabled := extractBool(data, "spec", "csi", "enabled")

	fmt.Printf("  NFS Gateway:  %s\n", enabledStr(nfsEnabled))
	if nfsEnabled {
		nfsImage := extractString(data, "spec", "nfs", "image")
		nfsPort := extractInt64(data, "spec", "nfs", "nodePort")
		exportPath := extractString(data, "spec", "nfs", "exportPath")
		if exportPath == "" {
			exportPath = "/"
		}
		fmt.Printf("    Image:      %s\n", nfsImage)
		fmt.Printf("    NodePort:   %d\n", nfsPort)
		fmt.Printf("    ExportPath: %s\n", exportPath)
	}

	fmt.Printf("  Web UI:       %s\n", enabledStr(webUIEnabled))
	if webUIEnabled {
		webuiPort := extractInt64(data, "spec", "interface", "port")
		webuiSvcType := extractString(data, "spec", "interface", "serviceType")
		fmt.Printf("    Port:        %d\n", webuiPort)
		fmt.Printf("    ServiceType: %s\n", webuiSvcType)
	}

	fmt.Printf("  Expose:       %s\n", enabledStr(exposeEnabled))
	if exposeEnabled {
		clientNP := extractInt64(data, "spec", "expose", "clientNodePort")
		adminNP := extractInt64(data, "spec", "expose", "adminNodePort")
		fmt.Printf("    Client NodePort: %d\n", clientNP)
		fmt.Printf("    Admin NodePort:  %d\n", adminNP)
	}

	fmt.Printf("  CSI Driver:   %s\n", enabledStr(csiEnabled))

	return nil
}

// --- helpers ---

func stringVal(v interface{}) string {
	s, _ := v.(string)
	return s
}

func enabledStr(v bool) string {
	if v {
		return "enabled"
	}
	return "disabled"
}

func extractBool(obj map[string]interface{}, keys ...string) bool {
	cur := interface{}(obj)
	for _, k := range keys {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return false
		}
		cur = m[k]
	}
	switch v := cur.(type) {
	case bool:
		return v
	case *bool:
		return v != nil && *v
	}
	return false
}

func extractMap(obj map[string]interface{}, keys ...string) map[string]interface{} {
	cur := interface{}(obj)
	for _, k := range keys {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = m[k]
	}
	m, _ := cur.(map[string]interface{})
	return m
}

func extractSlice(obj map[string]interface{}, keys ...string) []interface{} {
	cur := interface{}(obj)
	for _, k := range keys {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = m[k]
	}
	s, _ := cur.([]interface{})
	return s
}

func extractSliceRaw(obj map[string]interface{}, key string) []interface{} {
	v, _ := obj[key].([]interface{})
	return v
}

func renderLabels(m map[string]interface{}) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return strings.Join(parts, ",")
}
