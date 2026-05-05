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

package e2e

// Smoke tests for the kubectl-saunafs plugin.
//
// The tests require a running Kind cluster with a LeilFSCluster deployed.
// Start (or reset) the cluster with:
//
//	make kind-reset
//
// Then run the tests:
//
//	make test-plugin
//
// Optional env overrides:
//
//	PLUGIN_BIN      path to the plugin binary   (default: bin/kubectl-saunafs)
//	PLUGIN_CLUSTER  LeilFSCluster name          (default: leilfscluster-sample)
//	PLUGIN_NS       namespace                    (default: default)

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/henres/leilfs-operator/test/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// pluginEnv returns configuration from environment variables with defaults.
// The plugin binary path is resolved relative to the project root so that
// the tests can be run from any working directory.
func pluginEnv() (bin, cluster, ns string) {
	bin = os.Getenv("PLUGIN_BIN")
	if bin == "" {
		bin = "bin/kubectl-saunafs"
	}
	// Resolve relative paths against the project root (two directories up
	// from test/e2e/).
	if !filepath.IsAbs(bin) {
		projectDir, err := utils.GetProjectDir()
		if err == nil {
			bin = filepath.Join(projectDir, bin)
		}
	}
	cluster = os.Getenv("PLUGIN_CLUSTER")
	if cluster == "" {
		cluster = "leilfscluster-sample"
	}
	ns = os.Getenv("PLUGIN_NS")
	if ns == "" {
		ns = "default"
	}
	return
}

// runPlugin executes the plugin binary with the given arguments and returns
// combined stdout+stderr output and the error (if any).
func runPlugin(bin string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

var _ = Describe("kubectl-saunafs plugin", Ordered, func() {
	var bin, cluster, ns string

	BeforeAll(func() {
		bin, cluster, ns = pluginEnv()

		By("checking the plugin binary exists and is executable")
		info, err := os.Stat(bin)
		Expect(err).NotTo(HaveOccurred(), "plugin binary %q not found — run 'make build-plugin' first", bin)
		Expect(info.Mode()&0o111).NotTo(BeZero(), "plugin binary %q is not executable", bin)
	})

	// -------------------------------------------------------------------------
	// list
	// -------------------------------------------------------------------------
	Describe("list", func() {
		It("lists LeilFSClusters and includes the sample cluster", func() {
			out, err := runPlugin(bin, "-n", ns, "list")
			Expect(err).NotTo(HaveOccurred(), "kubectl-saunafs list failed:\n%s", out)
			Expect(out).To(ContainSubstring(cluster),
				"expected cluster %q in 'list' output", cluster)
		})

		It("lists all namespaces with -A", func() {
			out, err := runPlugin(bin, "list", "-A")
			Expect(err).NotTo(HaveOccurred(), "kubectl-saunafs list -A failed:\n%s", out)
			Expect(out).To(ContainSubstring(cluster))
		})

		It("shows NAMESPACE column when using -A", func() {
			out, err := runPlugin(bin, "list", "-A")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("NAMESPACE"))
		})
	})

	// -------------------------------------------------------------------------
	// status
	// -------------------------------------------------------------------------
	Describe("status", func() {
		It("prints cluster status without error", func() {
			out, err := runPlugin(bin, "-n", ns, "status", cluster)
			Expect(err).NotTo(HaveOccurred(), "kubectl-saunafs status failed:\n%s", out)
			Expect(out).NotTo(BeEmpty())
		})

		It("shows the cluster name in the output", func() {
			out, err := runPlugin(bin, "-n", ns, "status", cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring(cluster))
		})

		It("shows chunk-server count", func() {
			out, err := runPlugin(bin, "-n", ns, "status", cluster)
			Expect(err).NotTo(HaveOccurred())
			// The status output includes "Chunk Servers" or similar heading
			Expect(out).To(MatchRegexp(`(?i)chunk`))
		})

		It("returns an error for a non-existent cluster", func() {
			out, err := runPlugin(bin, "-n", ns, "status", "does-not-exist")
			Expect(err).To(HaveOccurred(), "expected error for missing cluster, got:\n%s", out)
		})
	})

	// -------------------------------------------------------------------------
	// topology
	// -------------------------------------------------------------------------
	Describe("topology", func() {
		It("prints topology without error", func() {
			out, err := runPlugin(bin, "-n", ns, "topology", cluster)
			Expect(err).NotTo(HaveOccurred(), "kubectl-saunafs topology failed:\n%s", out)
			Expect(out).NotTo(BeEmpty())
		})

		It("shows the master section", func() {
			out, err := runPlugin(bin, "-n", ns, "topology", cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(MatchRegexp(`(?i)master`))
		})

		It("shows chunk server entries", func() {
			out, err := runPlugin(bin, "-n", ns, "topology", cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(MatchRegexp(`(?i)chunk`))
		})
	})

	// -------------------------------------------------------------------------
	// goals
	// -------------------------------------------------------------------------
	Describe("goals", func() {
		It("prints goals without error", func() {
			out, err := runPlugin(bin, "-n", ns, "goals", cluster)
			Expect(err).NotTo(HaveOccurred(), "kubectl-saunafs goals failed:\n%s", out)
			Expect(out).NotTo(BeEmpty())
		})

		It("shows at least one goal entry", func() {
			out, err := runPlugin(bin, "-n", ns, "goals", cluster)
			Expect(err).NotTo(HaveOccurred())
			// Expect a line with an ID number and a name
			Expect(out).To(MatchRegexp(`\d+\s+\w+`))
		})

		It("shows the custom ec_4_2 goal defined in the sample", func() {
			out, err := runPlugin(bin, "-n", ns, "goals", cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("ec_4_2"))
		})
	})

	// -------------------------------------------------------------------------
	// logs
	// -------------------------------------------------------------------------
	Describe("logs", func() {
		It("fetches master logs without error", func() {
			out, err := runPlugin(bin, "-n", ns, "logs", cluster)
			Expect(err).NotTo(HaveOccurred(), "kubectl-saunafs logs failed:\n%s", out)
			Expect(out).NotTo(BeEmpty())
		})

		It("fetches last 5 lines from master with --tail", func() {
			out, err := runPlugin(bin, "-n", ns, "logs", cluster, "--tail", "5")
			Expect(err).NotTo(HaveOccurred(), "kubectl-saunafs logs --tail failed:\n%s", out)
			lines := strings.Split(strings.TrimSpace(out), "\n")
			Expect(len(lines)).To(BeNumerically("<=", 5+1), // +1 for header line printed to stderr
				"expected at most 5 log lines, got %d", len(lines))
		})

		It("fetches nfs component logs without error", func() {
			out, err := runPlugin(bin, "-n", ns, "logs", cluster, "--component", "nfs", "--tail", "10")
			Expect(err).NotTo(HaveOccurred(), "kubectl-saunafs logs --component nfs failed:\n%s", out)
		})

		It("fetches interface component logs without error", func() {
			out, err := runPlugin(bin, "-n", ns, "logs", cluster, "--component", "interface", "--tail", "10")
			Expect(err).NotTo(HaveOccurred(), "kubectl-saunafs logs --component interface failed:\n%s", out)
		})

		It("fetches a specific chunk server log with --component chunk --server", func() {
			out, err := runPlugin(bin, "-n", ns, "logs", cluster,
				"--component", "chunk",
				"--server", "worker1-hdd001",
				"--tail", "10")
			Expect(err).NotTo(HaveOccurred(), "kubectl-saunafs logs --component chunk failed:\n%s", out)
		})

		It("returns an error for unknown component", func() {
			out, err := runPlugin(bin, "-n", ns, "logs", cluster, "--component", "bogus")
			Expect(err).To(HaveOccurred(), "expected error for unknown component, got:\n%s", out)
		})
	})

	// -------------------------------------------------------------------------
	// admin  (uses an ephemeral pod with saunafs-client:latest)
	// -------------------------------------------------------------------------
	Describe("admin", Ordered, func() {
		// These tests spin up ephemeral pods — give them more time.
		SetDefaultEventuallyTimeout(2 * time.Minute)

		It("runs saunafs-admin info and returns cluster statistics", func() {
			out, err := runPlugin(bin, "-n", ns, "admin", cluster, "--", "info")
			Expect(err).NotTo(HaveOccurred(), "kubectl-saunafs admin info failed:\n%s", out)
			Expect(out).To(ContainSubstring("Total space"))
			Expect(out).To(ContainSubstring("Available space"))
		})

		It("runs list-chunkservers and shows connected chunk servers", func() {
			out, err := runPlugin(bin, "-n", ns, "admin", cluster, "--", "list-chunkservers")
			Expect(err).NotTo(HaveOccurred(), "kubectl-saunafs admin list-chunkservers failed:\n%s", out)
			// Expect at least one chunkserver entry (IP address pattern)
			Expect(out).To(MatchRegexp(`\d+\.\d+\.\d+\.\d+`))
		})

		It("runs ready-chunkservers-count and returns a number", func() {
			out, err := runPlugin(bin, "-n", ns, "admin", cluster, "--", "ready-chunkservers-count")
			Expect(err).NotTo(HaveOccurred(), "kubectl-saunafs admin ready-chunkservers-count failed:\n%s", out)
			Expect(out).To(MatchRegexp(`\d+`))
		})

		It("runs list-goals and shows the custom goal", func() {
			out, err := runPlugin(bin, "-n", ns, "admin", cluster, "--", "list-goals", "--pretty")
			Expect(err).NotTo(HaveOccurred(), "kubectl-saunafs admin list-goals failed:\n%s", out)
			Expect(out).To(ContainSubstring("ec_4_2"))
		})

		It("runs metadataserver-status and returns personality", func() {
			out, err := runPlugin(bin, "-n", ns, "admin", cluster, "--", "metadataserver-status")
			Expect(err).NotTo(HaveOccurred(), "kubectl-saunafs admin metadataserver-status failed:\n%s", out)
			Expect(out).To(MatchRegexp(`(?i)master|shadow|personality`))
		})
	})

	// -------------------------------------------------------------------------
	// filegoal  (uses a privileged FUSE pod with sfsmount)
	// -------------------------------------------------------------------------
	Describe("filegoal", Ordered, func() {
		// These tests spin up privileged FUSE pods — give them more time.
		SetDefaultEventuallyTimeout(3 * time.Minute)

		It("gets the goal of the root directory without error", func() {
			out, err := runPlugin(bin, "-n", ns, "filegoal", "get", cluster, "/")
			Expect(err).NotTo(HaveOccurred(), "kubectl-saunafs filegoal get / failed:\n%s", out)
			// output format: "/mnt/saunafs: <goal-name>"
			Expect(out).To(MatchRegexp(`/mnt/saunafs:\s+\S+`))
		})

		It("sets the goal of the root directory to ec_4_2", func() {
			out, err := runPlugin(bin, "-n", ns, "filegoal", "set", cluster, "ec_4_2", "/")
			Expect(err).NotTo(HaveOccurred(), "kubectl-saunafs filegoal set ec_4_2 / failed:\n%s", out)
			Expect(out).To(MatchRegexp(`/mnt/saunafs`))
		})

		It("gets the goal again and shows ec_4_2", func() {
			out, err := runPlugin(bin, "-n", ns, "filegoal", "get", cluster, "/")
			Expect(err).NotTo(HaveOccurred(), "kubectl-saunafs filegoal get / failed:\n%s", out)
			Expect(out).To(ContainSubstring("ec_4_2"))
		})

		It("returns an error for a non-existent cluster", func() {
			out, err := runPlugin(bin, "-n", ns, "filegoal", "get", "does-not-exist", "/")
			Expect(err).To(HaveOccurred(), "expected error for missing cluster, got:\n%s", out)
		})
	})
})
