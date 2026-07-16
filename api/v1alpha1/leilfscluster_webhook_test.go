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

package v1alpha1

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These are direct unit tests of the CustomValidator methods and the
// underlying validateLeilFSCluster logic — no envtest, no live webhook
// server. They construct LeilFSCluster objects in memory and call
// ValidateCreate/ValidateUpdate/ValidateDelete exactly as the admission
// webhook wiring would, covering the three ROADMAP rules:
//
//  1. spec.chunk.servers[].name uniqueness
//  2. no duplicate mountPaths (HostPath/ClaimName) within/across chunk servers
//  3. NodePort fields must be 0 or in [30000, 32767]

func validCluster() *LeilFSCluster {
	return &LeilFSCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
		Spec: LeilFSClusterSpec{
			Chunk: ChunkSpec{
				Servers: []ChunkServerSpec{
					{
						Name:     "cs1",
						NodeName: "node-a",
						MountPaths: []MountPath{
							{Path: "/mnt/hdd001", HostPath: "/data/hdd001"},
							{Path: "/mnt/hdd002", HostPath: "/data/hdd002"},
						},
					},
					{
						Name:     "cs2",
						NodeName: "node-b",
						MountPaths: []MountPath{
							// Same HostPath as cs1's first mount, but a different
							// node — this is the normal auto-discover/local-disk
							// case and must NOT be flagged.
							{Path: "/mnt/hdd001", HostPath: "/data/hdd001"},
						},
					},
				},
			},
			Expose: ExposeSpec{
				ClientNodePort: 30100,
				AdminNodePort:  30200,
			},
			NFS: NFSSpec{
				NodePort: 30300,
			},
		},
	}
}

func TestValidateCreate_ValidClusterIsAdmitted(t *testing.T) {
	v := &LeilFSClusterCustomValidator{}
	warnings, err := v.ValidateCreate(context.Background(), validCluster())
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got: %v", warnings)
	}
}

func TestValidateCreate_WrongTypeReturnsError(t *testing.T) {
	v := &LeilFSClusterCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), &LeilFSClusterList{})
	if err == nil {
		t.Fatal("expected an error for a non-LeilFSCluster object, got nil")
	}
}

func TestValidateCreate_DuplicateChunkServerName(t *testing.T) {
	cluster := validCluster()
	cluster.Spec.Chunk.Servers[1].Name = "cs1" // duplicate of Servers[0].Name

	v := &LeilFSClusterCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), cluster)
	if err == nil {
		t.Fatal("expected an error for duplicate chunk server names, got nil")
	}
	if !strings.Contains(err.Error(), "spec.chunk.servers[1].name") {
		t.Fatalf("expected error to reference spec.chunk.servers[1].name, got: %v", err)
	}
}

func TestValidateCreate_UniqueChunkServerNamesAdmitted(t *testing.T) {
	cluster := validCluster() // cs1, cs2 — already unique
	v := &LeilFSClusterCustomValidator{}
	if _, err := v.ValidateCreate(context.Background(), cluster); err != nil {
		t.Fatalf("expected unique chunk server names to be admitted, got: %v", err)
	}
}

func TestValidateCreate_DuplicateHostPathWithinSameServer(t *testing.T) {
	cluster := validCluster()
	cluster.Spec.Chunk.Servers[0].MountPaths = []MountPath{
		{Path: "/mnt/hdd001", HostPath: "/data/hdd001"},
		{Path: "/mnt/hdd002", HostPath: "/data/hdd001"}, // duplicate HostPath
	}

	v := &LeilFSClusterCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), cluster)
	if err == nil {
		t.Fatal("expected an error for duplicate hostPath within one chunk server, got nil")
	}
	if !strings.Contains(err.Error(), "hostPath") {
		t.Fatalf("expected error to reference hostPath, got: %v", err)
	}
}

func TestValidateCreate_DuplicateClaimNameWithinSameServer(t *testing.T) {
	cluster := validCluster()
	cluster.Spec.Chunk.Servers[0].MountPaths = []MountPath{
		{Path: "/mnt/hdd001", ClaimName: "pvc-a"},
		{Path: "/mnt/hdd002", ClaimName: "pvc-a"}, // duplicate ClaimName
	}

	v := &LeilFSClusterCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), cluster)
	if err == nil {
		t.Fatal("expected an error for duplicate claimName within one chunk server, got nil")
	}
	if !strings.Contains(err.Error(), "claimName") {
		t.Fatalf("expected error to reference claimName, got: %v", err)
	}
}

func TestValidateCreate_DuplicateClaimNameAcrossServers(t *testing.T) {
	cluster := validCluster()
	cluster.Spec.Chunk.Servers[0].MountPaths = []MountPath{{Path: "/mnt/hdd001", ClaimName: "shared-pvc"}}
	cluster.Spec.Chunk.Servers[1].MountPaths = []MountPath{{Path: "/mnt/hdd001", ClaimName: "shared-pvc"}}

	v := &LeilFSClusterCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), cluster)
	if err == nil {
		t.Fatal("expected an error for the same claimName reused across chunk servers, got nil")
	}
	if !strings.Contains(err.Error(), "claimName") {
		t.Fatalf("expected error to reference claimName, got: %v", err)
	}
}

func TestValidateCreate_DuplicateHostPathAcrossServersSameNode(t *testing.T) {
	cluster := validCluster()
	cluster.Spec.Chunk.Servers[0].NodeName = "shared-node"
	cluster.Spec.Chunk.Servers[0].MountPaths = []MountPath{{Path: "/mnt/hdd001", HostPath: "/data/hdd001"}}
	cluster.Spec.Chunk.Servers[1].NodeName = "shared-node" // same node as Servers[0]
	cluster.Spec.Chunk.Servers[1].MountPaths = []MountPath{{Path: "/mnt/hdd001", HostPath: "/data/hdd001"}}

	v := &LeilFSClusterCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), cluster)
	if err == nil {
		t.Fatal("expected an error for the same hostPath reused on the same node across chunk servers, got nil")
	}
	if !strings.Contains(err.Error(), "hostPath") {
		t.Fatalf("expected error to reference hostPath, got: %v", err)
	}
}

func TestValidateCreate_SameHostPathAcrossServersDifferentNodesAdmitted(t *testing.T) {
	// validCluster() already sets this up: cs1/node-a and cs2/node-b share
	// HostPath "/data/hdd001" — this is the normal multi-node deployment shape
	// and must be admitted.
	cluster := validCluster()

	v := &LeilFSClusterCustomValidator{}
	if _, err := v.ValidateCreate(context.Background(), cluster); err != nil {
		t.Fatalf("expected same hostPath on different nodes to be admitted, got: %v", err)
	}
}

func TestValidateCreate_NodePortInRangeAdmitted(t *testing.T) {
	tests := []struct {
		name  string
		apply func(*LeilFSCluster)
	}{
		{"clientNodePort at lower bound", func(c *LeilFSCluster) { c.Spec.Expose.ClientNodePort = 30000 }},
		{"clientNodePort at upper bound", func(c *LeilFSCluster) { c.Spec.Expose.ClientNodePort = 32767 }},
		{"adminNodePort mid range", func(c *LeilFSCluster) { c.Spec.Expose.AdminNodePort = 31000 }},
		{"nfs nodePort mid range", func(c *LeilFSCluster) { c.Spec.NFS.NodePort = 31500 }},
		{"clientNodePort unset (0) means auto-assign", func(c *LeilFSCluster) { c.Spec.Expose.ClientNodePort = 0 }},
		{"adminNodePort unset (0) means auto-assign", func(c *LeilFSCluster) { c.Spec.Expose.AdminNodePort = 0 }},
		{"nfs nodePort unset (0) means auto-assign", func(c *LeilFSCluster) { c.Spec.NFS.NodePort = 0 }},
	}

	v := &LeilFSClusterCustomValidator{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cluster := validCluster()
			tc.apply(cluster)
			if _, err := v.ValidateCreate(context.Background(), cluster); err != nil {
				t.Fatalf("expected NodePort to be admitted, got: %v", err)
			}
		})
	}
}

func TestValidateCreate_NodePortOutOfRangeRejected(t *testing.T) {
	tests := []struct {
		name      string
		apply     func(*LeilFSCluster)
		wantField string
	}{
		{"clientNodePort below range", func(c *LeilFSCluster) { c.Spec.Expose.ClientNodePort = 29999 }, "clientNodePort"},
		{"clientNodePort above range", func(c *LeilFSCluster) { c.Spec.Expose.ClientNodePort = 32768 }, "clientNodePort"},
		{"adminNodePort below range", func(c *LeilFSCluster) { c.Spec.Expose.AdminNodePort = 1024 }, "adminNodePort"},
		{"nfs nodePort above range", func(c *LeilFSCluster) { c.Spec.NFS.NodePort = 40000 }, "nodePort"},
	}

	v := &LeilFSClusterCustomValidator{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cluster := validCluster()
			tc.apply(cluster)
			_, err := v.ValidateCreate(context.Background(), cluster)
			if err == nil {
				t.Fatalf("expected an error for out-of-range NodePort, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Fatalf("expected error to reference %s, got: %v", tc.wantField, err)
			}
		})
	}
}

func TestValidateCreate_AggregatesMultipleViolations(t *testing.T) {
	cluster := validCluster()
	cluster.Spec.Chunk.Servers[1].Name = "cs1" // rule 1 violation
	cluster.Spec.Expose.ClientNodePort = 100   // rule 3 violation
	cluster.Spec.Chunk.Servers[0].MountPaths = []MountPath{
		{Path: "/mnt/hdd001", HostPath: "/data/hdd001"},
		{Path: "/mnt/hdd002", HostPath: "/data/hdd001"}, // rule 2 violation
	}

	v := &LeilFSClusterCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), cluster)
	if err == nil {
		t.Fatal("expected an aggregated error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"spec.chunk.servers[1].name", "hostPath", "clientNodePort"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected aggregated error to mention %q, got: %v", want, msg)
		}
	}
}

func TestValidateUpdate_AppliesSameRules(t *testing.T) {
	oldCluster := validCluster()
	newCluster := validCluster()
	newCluster.Spec.Chunk.Servers[1].Name = "cs1" // duplicate name introduced on update

	v := &LeilFSClusterCustomValidator{}
	_, err := v.ValidateUpdate(context.Background(), oldCluster, newCluster)
	if err == nil {
		t.Fatal("expected an error for duplicate chunk server names on update, got nil")
	}
	if !strings.Contains(err.Error(), "spec.chunk.servers[1].name") {
		t.Fatalf("expected error to reference spec.chunk.servers[1].name, got: %v", err)
	}
}

func TestValidateUpdate_ValidClusterAdmitted(t *testing.T) {
	oldCluster := validCluster()
	newCluster := validCluster()
	newCluster.Spec.Chunk.Servers[0].Label = "rack-a" // benign, unrelated change

	v := &LeilFSClusterCustomValidator{}
	if _, err := v.ValidateUpdate(context.Background(), oldCluster, newCluster); err != nil {
		t.Fatalf("expected valid update to be admitted, got: %v", err)
	}
}

func TestValidateDelete_AlwaysAdmitted(t *testing.T) {
	cluster := validCluster()
	// Even a cluster that would fail create/update validation must be
	// deletable.
	cluster.Spec.Chunk.Servers[1].Name = "cs1"

	v := &LeilFSClusterCustomValidator{}
	warnings, err := v.ValidateDelete(context.Background(), cluster)
	if err != nil {
		t.Fatalf("expected delete to always be admitted, got: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings on delete, got: %v", warnings)
	}
}
