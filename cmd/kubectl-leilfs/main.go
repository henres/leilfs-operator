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

// kubectl-leilfs is a kubectl plugin to inspect and manage LeilFSCluster resources.
//
// Usage:
//
//	kubectl leilfs list               List all LeilFSCluster resources
//	kubectl leilfs status   <name>    Show the status of a LeilFSCluster
//	kubectl leilfs topology <name>    Show master/chunkserver topology
//	kubectl leilfs goals    <name>    List the configured storage goals
//	kubectl leilfs logs     <name>    Stream logs from master or a chunkserver
//	kubectl leilfs admin    <name>    Execute saunafs-admin commands on the master pod
package main

import (
	"os"

	"github.com/henres/leilfs-operator/cmd/kubectl-leilfs/cmd"
)

func main() {
	if err := cmd.NewRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}
