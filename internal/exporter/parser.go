// Package exporter implements a Prometheus exporter for LeilFS clusters
// by executing `saunafs-admin <subcommand> --porcelain` against the
// local sfsmaster and translating each output into stable metrics.
//
// Column layouts mirror the upstream SaunaFS sources at
// src/admin/*_command.cc (printPorcelainMode branches). Each parser
// here documents the exact upstream order, so a future bump of the
// SaunaFS version can be cross-referenced quickly.
package exporter

import (
	"fmt"
	"strconv"
	"strings"
)

// Info is the parsed output of `saunafs-admin info --porcelain`.
//
// Upstream layout (src/admin/info_command.cc, run()):
//
//	version memoryUsage totalSpace availableSpace trashSpace
//	trashNodes reservedSpace reservedNodes allNodes dirNodes
//	fileNodes symlinkNodes chunks chunkCopies regularCopies
//
// 15 space-separated fields, no header line.
type Info struct {
	Version        string
	MemoryUsage    uint64
	TotalSpace     uint64
	AvailableSpace uint64
	TrashSpace     uint64
	TrashNodes     uint64
	ReservedSpace  uint64
	ReservedNodes  uint64
	AllNodes       uint64
	DirNodes       uint64
	FileNodes      uint64
	SymlinkNodes   uint64
	Chunks         uint64
	ChunkCopies    uint64
	// RegularCopies is reported as a deprecated duplicate of
	// ChunkCopies by upstream. Kept for completeness.
	RegularCopies uint64
}

// ParseInfo parses a single-line `info --porcelain` output.
func ParseInfo(out string) (*Info, error) {
	line := firstNonEmptyLine(out)
	if line == "" {
		return nil, fmt.Errorf("empty info output")
	}
	f := strings.Fields(line)
	if len(f) < 15 {
		return nil, fmt.Errorf("info: expected 15 fields, got %d in %q", len(f), line)
	}
	info := &Info{Version: f[0]}
	uints := []*uint64{
		&info.MemoryUsage, &info.TotalSpace, &info.AvailableSpace,
		&info.TrashSpace, &info.TrashNodes, &info.ReservedSpace,
		&info.ReservedNodes, &info.AllNodes, &info.DirNodes,
		&info.FileNodes, &info.SymlinkNodes, &info.Chunks,
		&info.ChunkCopies, &info.RegularCopies,
	}
	for i, p := range uints {
		v, err := strconv.ParseUint(f[i+1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("info field %d (%q): %w", i+1, f[i+1], err)
		}
		*p = v
	}
	return info, nil
}

// Chunkserver describes one connected chunkserver.
//
// Upstream layout (src/admin/list_chunkservers_command.cc):
//
//	ip:port version chunkscount usedspace totalspace
//	todelchunkscount todelusedspace todeltotalspace
//	errorcounter label
//
// Disconnected entries are reported by upstream as
//
//	ip:port - 0 0 0 0 0 0 0 -
//
// which we surface with Version=="-".
type Chunkserver struct {
	Address         string
	Version         string
	Chunks          uint64
	UsedSpace       uint64
	TotalSpace      uint64
	ToDelChunks     uint64
	ToDelUsedSpace  uint64
	ToDelTotalSpace uint64
	Errors          uint64
	Label           string
	Disconnected    bool
}

// ParseChunkservers parses `list-chunkservers --porcelain` output.
func ParseChunkservers(out string) ([]Chunkserver, error) {
	var result []Chunkserver
	for _, line := range nonEmptyLines(out) {
		f := strings.Fields(line)
		if len(f) < 10 {
			return nil, fmt.Errorf("list-chunkservers: expected 10 fields, got %d in %q", len(f), line)
		}
		cs := Chunkserver{Address: f[0], Version: f[1], Label: f[9]}
		if f[1] == "-" {
			cs.Disconnected = true
		}
		uints := []*uint64{
			&cs.Chunks, &cs.UsedSpace, &cs.TotalSpace,
			&cs.ToDelChunks, &cs.ToDelUsedSpace, &cs.ToDelTotalSpace,
			&cs.Errors,
		}
		for i, p := range uints {
			v, err := strconv.ParseUint(f[i+2], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("list-chunkservers field %d (%q): %w", i+2, f[i+2], err)
			}
			*p = v
		}
		result = append(result, cs)
	}
	return result, nil
}

// Metadataserver describes one master or shadow.
//
// Upstream layout (src/admin/list_metadataservers_command.cc):
//
//	ip port hostname personality serverStatus metadataVersion version
type Metadataserver struct {
	IP              string
	Port            uint64
	Hostname        string
	Personality     string // "master" | "shadow" | "<unknown>"
	ServerStatus    string // "running" | "connected" | "disconnected" | ...
	MetadataVersion uint64
	Version         string
}

// ParseMetadataservers parses `list-metadataservers --porcelain` output.
func ParseMetadataservers(out string) ([]Metadataserver, error) {
	var result []Metadataserver
	for _, line := range nonEmptyLines(out) {
		f := strings.Fields(line)
		if len(f) < 7 {
			return nil, fmt.Errorf("list-metadataservers: expected 7 fields, got %d in %q", len(f), line)
		}
		port, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("list-metadataservers port %q: %w", f[1], err)
		}
		mv, err := strconv.ParseUint(f[5], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("list-metadataservers metaversion %q: %w", f[5], err)
		}
		result = append(result, Metadataserver{
			IP:              f[0],
			Port:            port,
			Hostname:        f[2],
			Personality:     f[3],
			ServerStatus:    f[4],
			MetadataVersion: mv,
			Version:         f[6],
		})
	}
	return result, nil
}

// MetadataserverStatus is the parsed output of
// `saunafs-admin metadataserver-status --porcelain`:
//
//	personality\tserverStatus\tmetadataVersion
type MetadataserverStatus struct {
	Personality     string
	ServerStatus    string
	MetadataVersion uint64
}

// ParseMetadataserverStatus parses metadataserver-status porcelain.
func ParseMetadataserverStatus(out string) (*MetadataserverStatus, error) {
	line := firstNonEmptyLine(out)
	if line == "" {
		return nil, fmt.Errorf("empty metadataserver-status output")
	}
	f := strings.Fields(line)
	if len(f) < 3 {
		return nil, fmt.Errorf("metadataserver-status: expected 3 fields, got %d in %q", len(f), line)
	}
	mv, err := strconv.ParseUint(f[2], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("metadataserver-status metaversion %q: %w", f[2], err)
	}
	return &MetadataserverStatus{
		Personality:     f[0],
		ServerStatus:    f[1],
		MetadataVersion: mv,
	}, nil
}

// ChunksHealthReport groups availability / replication / deletion
// reports as emitted by `chunks-health --porcelain`.
type ChunksHealthReport struct {
	Availability []ChunksAvailability
	Replication  []ChunksReplication
	Deletion     []ChunksReplication
}

// ChunksAvailability is one AVA row.
//
// Upstream: `AVA goalName safe endangered lost`
type ChunksAvailability struct {
	Goal       string
	Safe       uint64
	Endangered uint64
	Lost       uint64
}

// ChunksReplication is one REP / DEL row.
//
// Upstream: `REP|DEL goalName <kMaxPartsCount counters>`
// kMaxPartsCount is 11 in upstream (parts 0..10+).
type ChunksReplication struct {
	Goal   string
	Counts []uint64 // length kMaxPartsCount
}

// ParseChunksHealth parses `chunks-health --porcelain` output.
func ParseChunksHealth(out string) (*ChunksHealthReport, error) {
	rep := &ChunksHealthReport{}
	for _, line := range nonEmptyLines(out) {
		f := strings.Fields(line)
		if len(f) < 2 {
			return nil, fmt.Errorf("chunks-health: too few fields in %q", line)
		}
		switch f[0] {
		case "AVA":
			if len(f) < 5 {
				return nil, fmt.Errorf("chunks-health AVA: expected 5 fields, got %d in %q", len(f), line)
			}
			a := ChunksAvailability{Goal: f[1]}
			vals := []*uint64{&a.Safe, &a.Endangered, &a.Lost}
			for i, p := range vals {
				v, err := strconv.ParseUint(f[i+2], 10, 64)
				if err != nil {
					return nil, fmt.Errorf("chunks-health AVA field %d (%q): %w", i+2, f[i+2], err)
				}
				*p = v
			}
			rep.Availability = append(rep.Availability, a)
		case "REP", "DEL":
			counts := make([]uint64, 0, len(f)-2)
			for i := 2; i < len(f); i++ {
				v, err := strconv.ParseUint(f[i], 10, 64)
				if err != nil {
					return nil, fmt.Errorf("chunks-health %s field %d (%q): %w", f[0], i, f[i], err)
				}
				counts = append(counts, v)
			}
			cr := ChunksReplication{Goal: f[1], Counts: counts}
			if f[0] == "REP" {
				rep.Replication = append(rep.Replication, cr)
			} else {
				rep.Deletion = append(rep.Deletion, cr)
			}
		default:
			return nil, fmt.Errorf("chunks-health: unknown row tag %q in %q", f[0], line)
		}
	}
	return rep, nil
}

// Disk describes one disk attached to a chunkserver.
//
// Upstream layout (src/admin/list_disks_command.cc, printPorcelainMode):
//
//	cs_addr path to_delete damaged scanning errorChunkId
//	errorTimeStamp total used chunksCount
//
// (without --verbose; verbose appends 3 × 8 stats trios — we ignore them
// for v1 to keep the metric surface manageable).
type Disk struct {
	ChunkserverAddress string
	Path               string
	ToDelete           bool
	Damaged            bool
	Scanning           bool
	ErrorChunkID       uint64
	ErrorTimestamp     uint64
	Total              uint64
	Used               uint64
	Chunks             uint64
}

// ParseDisks parses `list-disks --porcelain` output (non-verbose).
func ParseDisks(out string) ([]Disk, error) {
	var result []Disk
	for _, line := range nonEmptyLines(out) {
		f := strings.Fields(line)
		if len(f) < 10 {
			return nil, fmt.Errorf("list-disks: expected 10 fields, got %d in %q", len(f), line)
		}
		d := Disk{
			ChunkserverAddress: f[0],
			Path:               f[1],
			ToDelete:           f[2] == "yes",
			Damaged:            f[3] == "yes",
			Scanning:           f[4] == "yes",
		}
		uints := []*uint64{
			&d.ErrorChunkID, &d.ErrorTimestamp,
			&d.Total, &d.Used, &d.Chunks,
		}
		for i, p := range uints {
			v, err := strconv.ParseUint(f[i+5], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("list-disks field %d (%q): %w", i+5, f[i+5], err)
			}
			*p = v
		}
		result = append(result, d)
	}
	return result, nil
}

// Goal describes one storage goal definition.
//
// Upstream layout (src/admin/list_goals_command.cc):
//
//	id name definition
//
// definition may contain spaces but is escaped by
// escapePorcelainString in upstream — for v1 we recombine the trailing
// fields conservatively.
type Goal struct {
	ID         uint64
	Name       string
	Definition string
}

// ParseGoals parses `list-goals --porcelain` output.
func ParseGoals(out string) ([]Goal, error) {
	var result []Goal
	for _, line := range nonEmptyLines(out) {
		// Upstream escapes any whitespace/special chars in the
		// definition before printing, so plain Fields() splitting on
		// space leaves the first two columns clean and the rest as
		// the (escaped) definition. We rejoin from index 2.
		f := strings.Fields(line)
		if len(f) < 3 {
			return nil, fmt.Errorf("list-goals: expected >= 3 fields, got %d in %q", len(f), line)
		}
		id, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("list-goals id %q: %w", f[0], err)
		}
		def := strings.Join(f[2:], " ")
		// Upstream wraps the human-readable definition in literal
		// quotes (e.g. `"1: $std _"`) — strip them so the label
		// value is clean.
		def = strings.TrimPrefix(def, "\"")
		def = strings.TrimSuffix(def, "\"")
		result = append(result, Goal{
			ID:         id,
			Name:       f[1],
			Definition: def,
		})
	}
	return result, nil
}

// Mount is one connected FUSE/NFS session.
//
// Upstream layout (src/admin/list_mounts_command.cc, non-verbose):
//
//	sessionId peerIp info(=mountPoint) version path
//	rootuid rootgid mapalluid mapallgid
//	readonly restrictedIp ignoregid allCanChangeQuota mapAll
type Mount struct {
	SessionID         uint64
	PeerIP            string
	Info              string // mount point identifier
	Version           string
	Path              string
	RootUID           uint64
	RootGID           uint64
	MapAllUID         uint64
	MapAllGID         uint64
	ReadOnly          bool
	RestrictedIP      bool
	IgnoreGID         bool
	AllCanChangeQuota bool
	MapAll            bool
}

// ParseMounts parses `list-mounts --porcelain` output (non-verbose).
func ParseMounts(out string) ([]Mount, error) {
	var result []Mount
	for _, line := range nonEmptyLines(out) {
		f := strings.Fields(line)
		if len(f) < 14 {
			return nil, fmt.Errorf("list-mounts: expected 14 fields, got %d in %q", len(f), line)
		}
		sid, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("list-mounts sessionId %q: %w", f[0], err)
		}
		uintFields := []struct {
			idx int
			p   *uint64
		}{}
		m := Mount{
			SessionID: sid,
			PeerIP:    f[1],
			Info:      f[2],
			Version:   f[3],
			Path:      f[4],
		}
		uintFields = append(uintFields,
			struct {
				idx int
				p   *uint64
			}{5, &m.RootUID},
			struct {
				idx int
				p   *uint64
			}{6, &m.RootGID},
			struct {
				idx int
				p   *uint64
			}{7, &m.MapAllUID},
			struct {
				idx int
				p   *uint64
			}{8, &m.MapAllGID},
		)
		for _, uf := range uintFields {
			v, err := strconv.ParseUint(f[uf.idx], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("list-mounts field %d (%q): %w", uf.idx, f[uf.idx], err)
			}
			*uf.p = v
		}
		m.ReadOnly = f[9] == "yes"
		m.RestrictedIP = f[10] == "yes"
		m.IgnoreGID = f[11] == "yes"
		m.AllCanChangeQuota = f[12] == "yes"
		m.MapAll = f[13] == "yes"
		result = append(result, m)
	}
	return result, nil
}

// ParseReadyChunkserversCount parses the bare integer output of
// `saunafs-admin ready-chunkservers-count`.
func ParseReadyChunkserversCount(out string) (uint64, error) {
	line := firstNonEmptyLine(out)
	if line == "" {
		return 0, fmt.Errorf("empty ready-chunkservers-count output")
	}
	v, err := strconv.ParseUint(strings.TrimSpace(line), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("ready-chunkservers-count %q: %w", line, err)
	}
	return v, nil
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
	}
	return out
}

func firstNonEmptyLine(s string) string {
	lines := nonEmptyLines(s)
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}
