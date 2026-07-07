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

package utils

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2" //nolint:golint,revive
)

// KubeContext returns the kubectl context the e2e suite must target. It
// defaults to "sfs-lima", the shared Lima-VM k3s test cluster documented in
// the workspace AGENTS.md, and must never silently fall back to whatever
// context happens to be current on the machine running the tests -- this
// workspace's kubectl config also has unrelated shared/corporate clusters
// registered, and one of those is typically the ambient default context.
func KubeContext() string {
	if c := os.Getenv("E2E_KUBE_CONTEXT"); c != "" {
		return c
	}
	return "sfs-lima"
}

// KubectlCommand builds an *exec.Cmd for kubectl that always pins --context
// explicitly via KubeContext, so e2e commands never depend on the ambient
// default kubectl context.
func KubectlCommand(args ...string) *exec.Cmd {
	full := append([]string{"--context", KubeContext()}, args...)
	return exec.Command("kubectl", full...)
}

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) ([]byte, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		fmt.Fprintf(GinkgoWriter, "chdir dir: %s\n", err)
	}

	cmd.Env = append(os.Environ(),
		"GO111MODULE=on",
		// Pin any Makefile target invoked here that shells out via
		// $(KUBECTL) (e.g. "make install" / "make deploy") to the sfs-lima
		// context explicitly, instead of letting it fall back to the
		// ambient default kubectl context.
		fmt.Sprintf("KUBECTL=kubectl --context %s", KubeContext()),
	)
	command := strings.Join(cmd.Args, " ")
	fmt.Fprintf(GinkgoWriter, "running: %s\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s failed with error: (%v) %s", command, err, string(output))
	}

	return output, nil
}

// LoadImagesIntoCluster imports the locally built operator image(s) into
// containerd on every sfs-lima node. There is no "kind load" equivalent for
// a k3s-on-Lima cluster: images must be docker-built locally first (with
// the ghcr.io/henres/... :dev tags) and then streamed into each VM's
// containerd via sfs-test-env/scripts/load-images.sh, which loads a fixed
// list of dev images onto sfs-cp/sfs-w1/sfs-w2/sfs-w3.
func LoadImagesIntoCluster() error {
	projectDir, err := GetProjectDir()
	if err != nil {
		return err
	}
	// sfs-test-env is a sibling checkout at the workspace root:
	//   <workspace>/leilfs-operator, <workspace>/localdisk-operator, <workspace>/sfs-test-env
	script := filepath.Join(projectDir, "..", "sfs-test-env", "scripts", "load-images.sh")
	cmd := exec.Command("bash", script)
	_, err = Run(cmd)
	return err
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, err
	}
	wd = strings.Replace(wd, "/test/e2e", "", -1)
	return wd, nil
}
