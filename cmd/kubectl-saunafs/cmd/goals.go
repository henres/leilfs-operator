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
)

func newGoalsCmd(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "goals <cluster-name>",
		Short: "List the storage goals configured for a LeilFSCluster",
		Long: `Display the list of LeilFS storage goals defined in a LeilFSCluster's spec.

Goals control how many copies (replication) or how many data/parity shards
(erasure coding) are maintained for each stored file.

Examples:
  kubectl saunafs goals my-cluster
  kubectl saunafs goals my-cluster -n my-namespace`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runGoals(opts, args[0])
		},
	}
}

func runGoals(opts *rootOptions, name string) error {
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
		return fmt.Errorf("getting LeilFSCluster %q in namespace %q: %w", name, ns, err)
	}
	data := obj.Object

	goals := extractSlice(data, "spec", "goals")
	if len(goals) == 0 {
		fmt.Printf("No custom goals defined for LeilFSCluster %q.\n", name)
		fmt.Println("LeilFS will use its built-in defaults (goals 1–9).")
		return nil
	}

	fmt.Printf("Storage goals for LeilFSCluster %q (namespace: %s)\n\n", name, ns)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tTYPE\tCONFIG\tDEFAULT")

	for _, g := range goals {
		gm, ok := g.(map[string]interface{})
		if !ok {
			continue
		}

		id := extractInt64(gm, "id")
		gname := stringVal(gm["name"])
		isDefault := extractBool(gm, "default")

		var goalType, config string

		// Check for erasure coding.
		ec, hasEC := gm["ec"].(map[string]interface{})
		if hasEC {
			goalType = "erasure-coding"
			dp := extractInt64(ec, "dataParts")
			pp := extractInt64(ec, "parityParts")
			config = fmt.Sprintf("ec(%d,%d)  [%d data + %d parity, min %d chunk servers]",
				dp, pp, dp, pp, dp+pp)
		} else {
			goalType = "replication"
			rep := extractInt64(gm, "replication")
			if rep == 0 {
				rep = 1
			}
			config = fmt.Sprintf("%d cop%s", rep, pluralSuffix(int(rep), "y", "ies"))
		}

		defaultStr := ""
		if isDefault {
			defaultStr = "yes (cluster default)"
		}

		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", id, gname, goalType, config, defaultStr)
	}
	w.Flush()

	// Hint about what the sfsgoals.cfg will look like.
	fmt.Println()
	fmt.Println("sfsgoals.cfg representation:")
	fmt.Println()
	for _, g := range goals {
		gm, ok := g.(map[string]interface{})
		if !ok {
			continue
		}
		id := extractInt64(gm, "id")
		gname := stringVal(gm["name"])
		var line string
		ec, hasEC := gm["ec"].(map[string]interface{})
		if hasEC {
			dp := extractInt64(ec, "dataParts")
			pp := extractInt64(ec, "parityParts")
			line = fmt.Sprintf("%d %s : ec(%d, %d)", id, gname, dp, pp)
		} else {
			rep := extractInt64(gm, "replication")
			if rep == 0 {
				rep = 1
			}
			copies := ""
			for i := int64(0); i < rep; i++ {
				if i > 0 {
					copies += " "
				}
				copies += "_"
			}
			line = fmt.Sprintf("%d %s : %s", id, gname, copies)
		}
		fmt.Printf("  %s\n", line)
	}

	return nil
}

func pluralSuffix(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
