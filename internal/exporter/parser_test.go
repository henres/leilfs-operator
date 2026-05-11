package exporter

import (
	"testing"
)

// Real porcelain captures from sfs-lima (SaunaFS v5.9.0) on an empty
// cluster with EC 4+2 default goal. Used as fixtures so parser changes
// can be caught without a live cluster.

const infoFixture = `5.9.0 177553408 0 0 0 0 0 0 2 1 1 0 0 0 0
`

const chunkserversFixture = `10.42.4.127:9422 5.9.0 0 0 0 0 0 0 0 lima_sfs_w3
10.42.4.129:9422 5.9.0 0 0 0 0 0 0 0 lima_sfs_w3
10.42.1.39:9422 5.9.0 0 0 0 0 0 0 0 lima_sfs_w1
10.42.2.203:9422 5.9.0 0 0 0 0 0 0 0 lima_sfs_w2
10.42.1.40:9422 5.9.0 0 0 0 0 0 0 0 lima_sfs_w1
10.42.2.198:9422 5.9.0 0 0 0 0 0 0 0 lima_sfs_w2
`

const chunkserverDisconnectedFixture = `10.42.4.127:9422 - 0 0 0 0 0 0 0 -
`

const metadataserversFixture = `10.43.194.85 9421 leilfscluster-sample-master-0 master running 7 5.9.0
10.42.4.128 9421 leilfscluster-sample-master-1 shadow connected 7 5.9.0
`

const metadataserverStatusFixture = "master\trunning\t7\n"

const chunksHealthFixture = `AVA 1 0 0 0
AVA ec_4_2 0 0 0
REP 1 0 0 0 0 0 0 0 0 0 0 0
DEL 1 0 0 0 0 0 0 0 0 0 0 0
`

const goalsFixture = `1 1 "1: $std _"
10 ec_4_2 "ec_4_2: $ec(4,2) {_ _ _ _ _ _}"
`

const mountsFixture = `3 10.42.0.43 nfs-ganesha 5.9.0 / 0 0 999 999 no yes no no no
`

const readyCountFixture = "0\n"

func TestParseInfo(t *testing.T) {
	info, err := ParseInfo(infoFixture)
	if err != nil {
		t.Fatalf("ParseInfo: %v", err)
	}
	if info.Version != "5.9.0" {
		t.Errorf("Version = %q, want 5.9.0", info.Version)
	}
	if info.MemoryUsage != 177553408 {
		t.Errorf("MemoryUsage = %d, want 177553408", info.MemoryUsage)
	}
	if info.AllNodes != 2 || info.DirNodes != 1 || info.FileNodes != 1 {
		t.Errorf("nodes mismatch: all=%d dir=%d file=%d", info.AllNodes, info.DirNodes, info.FileNodes)
	}
	if info.Chunks != 0 || info.ChunkCopies != 0 {
		t.Errorf("chunks/copies want zero, got %d/%d", info.Chunks, info.ChunkCopies)
	}
}

func TestParseChunkservers(t *testing.T) {
	css, err := ParseChunkservers(chunkserversFixture)
	if err != nil {
		t.Fatalf("ParseChunkservers: %v", err)
	}
	if len(css) != 6 {
		t.Fatalf("len=%d, want 6", len(css))
	}
	if css[0].Address != "10.42.4.127:9422" || css[0].Label != "lima_sfs_w3" {
		t.Errorf("first cs mismatch: %+v", css[0])
	}
	for _, cs := range css {
		if cs.Disconnected {
			t.Errorf("unexpected disconnected: %+v", cs)
		}
	}
}

func TestParseChunkserversDisconnected(t *testing.T) {
	css, err := ParseChunkservers(chunkserverDisconnectedFixture)
	if err != nil {
		t.Fatalf("ParseChunkservers: %v", err)
	}
	if len(css) != 1 || !css[0].Disconnected {
		t.Errorf("expected one disconnected cs, got %+v", css)
	}
}

func TestParseMetadataservers(t *testing.T) {
	ms, err := ParseMetadataservers(metadataserversFixture)
	if err != nil {
		t.Fatalf("ParseMetadataservers: %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("len=%d, want 2", len(ms))
	}
	if ms[0].Personality != "master" || ms[0].ServerStatus != "running" || ms[0].MetadataVersion != 7 {
		t.Errorf("master mismatch: %+v", ms[0])
	}
	if ms[1].Personality != "shadow" || ms[1].ServerStatus != "connected" {
		t.Errorf("shadow mismatch: %+v", ms[1])
	}
}

func TestParseMetadataserverStatus(t *testing.T) {
	s, err := ParseMetadataserverStatus(metadataserverStatusFixture)
	if err != nil {
		t.Fatalf("ParseMetadataserverStatus: %v", err)
	}
	if s.Personality != "master" || s.ServerStatus != "running" || s.MetadataVersion != 7 {
		t.Errorf("status mismatch: %+v", s)
	}
}

func TestParseChunksHealth(t *testing.T) {
	rep, err := ParseChunksHealth(chunksHealthFixture)
	if err != nil {
		t.Fatalf("ParseChunksHealth: %v", err)
	}
	if len(rep.Availability) != 2 {
		t.Errorf("availability len=%d, want 2", len(rep.Availability))
	}
	if rep.Availability[1].Goal != "ec_4_2" {
		t.Errorf("availability[1].Goal = %q", rep.Availability[1].Goal)
	}
	if len(rep.Replication) != 1 || len(rep.Replication[0].Counts) != 11 {
		t.Errorf("replication mismatch: %+v", rep.Replication)
	}
	if len(rep.Deletion) != 1 {
		t.Errorf("deletion mismatch: %+v", rep.Deletion)
	}
}

func TestParseGoals(t *testing.T) {
	gs, err := ParseGoals(goalsFixture)
	if err != nil {
		t.Fatalf("ParseGoals: %v", err)
	}
	if len(gs) != 2 {
		t.Fatalf("len=%d, want 2", len(gs))
	}
	if gs[0].ID != 1 || gs[0].Name != "1" {
		t.Errorf("goal[0]: %+v", gs[0])
	}
	if gs[1].ID != 10 || gs[1].Name != "ec_4_2" {
		t.Errorf("goal[1]: %+v", gs[1])
	}
}

func TestParseMounts(t *testing.T) {
	ms, err := ParseMounts(mountsFixture)
	if err != nil {
		t.Fatalf("ParseMounts: %v", err)
	}
	if len(ms) != 1 {
		t.Fatalf("len=%d, want 1", len(ms))
	}
	m := ms[0]
	if m.SessionID != 3 || m.PeerIP != "10.42.0.43" || m.Info != "nfs-ganesha" || m.Version != "5.9.0" {
		t.Errorf("mount mismatch: %+v", m)
	}
	if !m.RestrictedIP || m.ReadOnly || m.IgnoreGID || m.AllCanChangeQuota || m.MapAll {
		t.Errorf("flags mismatch: %+v", m)
	}
}

func TestParseReadyChunkserversCount(t *testing.T) {
	v, err := ParseReadyChunkserversCount(readyCountFixture)
	if err != nil {
		t.Fatalf("ParseReadyChunkserversCount: %v", err)
	}
	if v != 0 {
		t.Errorf("got %d, want 0", v)
	}
}

func TestParseInfoBadInput(t *testing.T) {
	if _, err := ParseInfo(""); err == nil {
		t.Error("expected error on empty input")
	}
	if _, err := ParseInfo("5.9.0 1 2 3"); err == nil {
		t.Error("expected error on short input")
	}
	if _, err := ParseInfo("5.9.0 not-a-number 0 0 0 0 0 0 0 0 0 0 0 0 0"); err == nil {
		t.Error("expected error on non-numeric field")
	}
}
