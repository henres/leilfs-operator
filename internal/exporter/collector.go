package exporter

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
)

// CommandRunner abstracts execution of `saunafs-admin` subcommands so
// the collector can be unit-tested without forking real binaries.
type CommandRunner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// ExecRunner is the production CommandRunner: it forks
// `saunafs-admin <args>` and returns its stdout.
type ExecRunner struct {
	Binary string // defaults to "saunafs-admin"
}

// Run executes saunafs-admin with the supplied arguments under the
// given context (which carries the scrape timeout).
func (r *ExecRunner) Run(ctx context.Context, args ...string) (string, error) {
	bin := r.Binary
	if bin == "" {
		bin = "saunafs-admin"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.Output()
	if err != nil {
		// Surface stderr to logs to make debugging much easier
		// (especially "Wrong usage" / "Connection refused" / TLS).
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return string(out), fmt.Errorf("%s %v: %w: %s",
				bin, args, err, string(exitErr.Stderr))
		}
		return string(out), fmt.Errorf("%s %v: %w", bin, args, err)
	}
	return string(out), nil
}

// Collector is a Prometheus collector that, on each scrape, executes a
// fixed set of `saunafs-admin` subcommands in parallel against the
// local sfsmaster and emits filesystem-level metrics under the
// `leilfs_fs_*` namespace.
//
// All metrics are described as Desc only (no static GaugeVec) so we can
// safely reuse them across HA failover (no stale series from a former
// active master once the local sidecar starts producing fresh values
// after a Pod restart).
type Collector struct {
	runner     CommandRunner
	masterHost string
	masterPort string
	timeout    time.Duration
	logger     logr.Logger

	// scrape outcome
	upDesc             *prometheus.Desc
	scrapeErrorsDesc   *prometheus.Desc
	scrapeDurationDesc *prometheus.Desc

	// info
	infoDesc        *prometheus.Desc
	memoryDesc      *prometheus.Desc
	spaceDesc       *prometheus.Desc
	objectsDesc     *prometheus.Desc
	chunksTotalDesc *prometheus.Desc
	chunkCopiesDesc *prometheus.Desc

	// chunkservers
	csInfoDesc       *prometheus.Desc
	csChunksDesc     *prometheus.Desc
	csUsedSpaceDesc  *prometheus.Desc
	csTotalSpaceDesc *prometheus.Desc
	csErrorsDesc     *prometheus.Desc
	csToDelChunks    *prometheus.Desc
	csToDelUsed      *prometheus.Desc
	csToDelTotal     *prometheus.Desc

	readyCSDesc *prometheus.Desc

	// metadataservers
	msInfoDesc    *prometheus.Desc
	msMetaVerDesc *prometheus.Desc
	msStatusDesc  *prometheus.Desc

	// chunks-health
	chAvaSafeDesc   *prometheus.Desc
	chAvaEndangered *prometheus.Desc
	chAvaLostDesc   *prometheus.Desc
	chRepDesc       *prometheus.Desc
	chDelDesc       *prometheus.Desc

	// disks
	diskTotalDesc  *prometheus.Desc
	diskUsedDesc   *prometheus.Desc
	diskChunksDesc *prometheus.Desc
	diskFlagsDesc  *prometheus.Desc

	// goals
	goalInfoDesc *prometheus.Desc

	// mounts
	mountInfoDesc   *prometheus.Desc
	mountsTotalDesc *prometheus.Desc

	// concurrency
	mu sync.Mutex
}

// CollectorOptions configures a Collector.
type CollectorOptions struct {
	Runner     CommandRunner // defaults to &ExecRunner{}
	MasterHost string        // defaults to "127.0.0.1"
	MasterPort string        // defaults to "9421"
	Timeout    time.Duration // per-subcommand timeout; default 3s
	Logger     logr.Logger
}

// NewCollector constructs a Collector with stable metric descriptors.
func NewCollector(opts CollectorOptions) *Collector {
	c := &Collector{
		runner:     opts.Runner,
		masterHost: opts.MasterHost,
		masterPort: opts.MasterPort,
		timeout:    opts.Timeout,
		logger:     opts.Logger,
	}
	if c.runner == nil {
		c.runner = &ExecRunner{}
	}
	if c.masterHost == "" {
		c.masterHost = "127.0.0.1"
	}
	if c.masterPort == "" {
		c.masterPort = "9421"
	}
	if c.timeout == 0 {
		c.timeout = 3 * time.Second
	}

	c.upDesc = prometheus.NewDesc(
		"leilfs_fs_up",
		"Whether the last scrape of the LeilFS filesystem-level metrics succeeded (1) or not (0).",
		nil, nil)
	c.scrapeErrorsDesc = prometheus.NewDesc(
		"leilfs_fs_scrape_errors_total",
		"Number of saunafs-admin subcommands that returned an error during the last scrape, by subcommand.",
		[]string{"subcommand"}, nil)
	c.scrapeDurationDesc = prometheus.NewDesc(
		"leilfs_fs_scrape_duration_seconds",
		"Wall-time duration of the last scrape, by subcommand.",
		[]string{"subcommand"}, nil)

	c.infoDesc = prometheus.NewDesc(
		"leilfs_fs_info",
		"Constant 1 gauge carrying static cluster identifiers as labels (saunafs version).",
		[]string{"version"}, nil)
	c.memoryDesc = prometheus.NewDesc(
		"leilfs_fs_master_memory_bytes",
		"Memory usage of the sfsmaster process as reported by `saunafs-admin info`.",
		nil, nil)
	c.spaceDesc = prometheus.NewDesc(
		"leilfs_fs_space_bytes",
		"Aggregate space across all chunkservers, by kind (total, available, trash, reserved).",
		[]string{"kind"}, nil)
	c.objectsDesc = prometheus.NewDesc(
		"leilfs_fs_objects_total",
		"Filesystem object counts, by kind (all, dirs, files, symlinks, trash, reserved).",
		[]string{"kind"}, nil)
	c.chunksTotalDesc = prometheus.NewDesc(
		"leilfs_fs_chunks_total",
		"Total number of chunks tracked by the master.",
		nil, nil)
	c.chunkCopiesDesc = prometheus.NewDesc(
		"leilfs_fs_chunk_copies_total",
		"Total number of chunk copies (across all goals) tracked by the master.",
		nil, nil)

	csLabels := []string{"address", "label"}
	c.csInfoDesc = prometheus.NewDesc(
		"leilfs_fs_chunkserver_info",
		"Constant 1 gauge per connected chunkserver, with version and disconnected status as labels.",
		[]string{"address", "label", "version", "disconnected"}, nil)
	c.csChunksDesc = prometheus.NewDesc(
		"leilfs_fs_chunkserver_chunks",
		"Number of chunks stored on a chunkserver.",
		csLabels, nil)
	c.csUsedSpaceDesc = prometheus.NewDesc(
		"leilfs_fs_chunkserver_used_bytes",
		"Used disk space on a chunkserver, in bytes.",
		csLabels, nil)
	c.csTotalSpaceDesc = prometheus.NewDesc(
		"leilfs_fs_chunkserver_total_bytes",
		"Total disk space on a chunkserver, in bytes.",
		csLabels, nil)
	c.csErrorsDesc = prometheus.NewDesc(
		"leilfs_fs_chunkserver_errors_total",
		"Cumulative error counter reported by a chunkserver.",
		csLabels, nil)
	c.csToDelChunks = prometheus.NewDesc(
		"leilfs_fs_chunkserver_to_delete_chunks",
		"Number of chunks marked for deletion on a chunkserver.",
		csLabels, nil)
	c.csToDelUsed = prometheus.NewDesc(
		"leilfs_fs_chunkserver_to_delete_used_bytes",
		"Used space on disks marked for deletion on a chunkserver.",
		csLabels, nil)
	c.csToDelTotal = prometheus.NewDesc(
		"leilfs_fs_chunkserver_to_delete_total_bytes",
		"Total space on disks marked for deletion on a chunkserver.",
		csLabels, nil)

	c.readyCSDesc = prometheus.NewDesc(
		"leilfs_fs_chunkservers_ready",
		"Number of chunkservers currently ready to be written to.",
		nil, nil)

	c.msInfoDesc = prometheus.NewDesc(
		"leilfs_fs_metadataserver_info",
		"Constant 1 gauge per metadata server, with personality (master|shadow), status, version.",
		[]string{"ip", "port", "hostname", "personality", "status", "version"}, nil)
	c.msMetaVerDesc = prometheus.NewDesc(
		"leilfs_fs_metadataserver_metadata_version",
		"Current metadata version tracked by a metadata server.",
		[]string{"ip", "personality"}, nil)
	c.msStatusDesc = prometheus.NewDesc(
		"leilfs_fs_metadataserver_status",
		"1 if the metadata server is in its expected good state (master=running, shadow=connected), 0 otherwise.",
		[]string{"ip", "personality"}, nil)

	c.chAvaSafeDesc = prometheus.NewDesc(
		"leilfs_fs_chunks_safe",
		"Number of chunks fully available (safe) per goal.",
		[]string{"goal"}, nil)
	c.chAvaEndangered = prometheus.NewDesc(
		"leilfs_fs_chunks_endangered",
		"Number of chunks with fewer copies than goal but still available (endangered) per goal.",
		[]string{"goal"}, nil)
	c.chAvaLostDesc = prometheus.NewDesc(
		"leilfs_fs_chunks_lost",
		"Number of chunks not available anywhere (lost) per goal.",
		[]string{"goal"}, nil)
	c.chRepDesc = prometheus.NewDesc(
		"leilfs_fs_chunks_pending_replication",
		"Number of chunks that need replication, by goal and replication-bucket (number of existing copies, 0..10+).",
		[]string{"goal", "copies"}, nil)
	c.chDelDesc = prometheus.NewDesc(
		"leilfs_fs_chunks_pending_deletion",
		"Number of chunks that need deletion, by goal and bucket.",
		[]string{"goal", "copies"}, nil)

	diskLabels := []string{"chunkserver", "path"}
	c.diskTotalDesc = prometheus.NewDesc(
		"leilfs_fs_disk_total_bytes",
		"Total space on a single chunkserver disk, in bytes.",
		diskLabels, nil)
	c.diskUsedDesc = prometheus.NewDesc(
		"leilfs_fs_disk_used_bytes",
		"Used space on a single chunkserver disk, in bytes.",
		diskLabels, nil)
	c.diskChunksDesc = prometheus.NewDesc(
		"leilfs_fs_disk_chunks",
		"Number of chunks stored on a single chunkserver disk.",
		diskLabels, nil)
	c.diskFlagsDesc = prometheus.NewDesc(
		"leilfs_fs_disk_flag",
		"1 if the named flag (to_delete|damaged|scanning) is set on a disk, 0 otherwise.",
		[]string{"chunkserver", "path", "flag"}, nil)

	c.goalInfoDesc = prometheus.NewDesc(
		"leilfs_fs_goal_info",
		"Constant 1 gauge per storage goal, exposing id, name and definition as labels.",
		[]string{"id", "name", "definition"}, nil)

	c.mountInfoDesc = prometheus.NewDesc(
		"leilfs_fs_mount_info",
		"Constant 1 gauge per mounted session, with session id, peer IP, mount info and flags as labels.",
		[]string{"session_id", "peer_ip", "info", "version", "path", "read_only", "map_all"}, nil)
	c.mountsTotalDesc = prometheus.NewDesc(
		"leilfs_fs_mounts_total",
		"Total number of mounted sessions currently connected to the master.",
		nil, nil)

	return c
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.upDesc, c.scrapeErrorsDesc, c.scrapeDurationDesc,
		c.infoDesc, c.memoryDesc, c.spaceDesc, c.objectsDesc,
		c.chunksTotalDesc, c.chunkCopiesDesc,
		c.csInfoDesc, c.csChunksDesc, c.csUsedSpaceDesc, c.csTotalSpaceDesc,
		c.csErrorsDesc, c.csToDelChunks, c.csToDelUsed, c.csToDelTotal,
		c.readyCSDesc,
		c.msInfoDesc, c.msMetaVerDesc, c.msStatusDesc,
		c.chAvaSafeDesc, c.chAvaEndangered, c.chAvaLostDesc,
		c.chRepDesc, c.chDelDesc,
		c.diskTotalDesc, c.diskUsedDesc, c.diskChunksDesc, c.diskFlagsDesc,
		c.goalInfoDesc,
		c.mountInfoDesc, c.mountsTotalDesc,
	} {
		ch <- d
	}
}

// scrapeResult records the outcome of one saunafs-admin invocation.
type scrapeResult struct {
	out      string
	err      error
	duration time.Duration
}

// runCmd executes a single saunafs-admin subcommand and records timing.
func (c *Collector) runCmd(ctx context.Context, args ...string) scrapeResult {
	start := time.Now()
	out, err := c.runner.Run(ctx, args...)
	return scrapeResult{out: out, err: err, duration: time.Since(start)}
}

// Collect implements prometheus.Collector. It runs the saunafs-admin
// subcommands in parallel; each is wrapped in its own deadline so a
// single slow command cannot stall the whole scrape.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Build the base "[<master> <port>]" tail used by every command.
	tail := []string{c.masterHost, c.masterPort}
	porcelain := "--porcelain"

	// Subcommands to run in parallel. Most accept --porcelain; the
	// odd ones out (`ready-chunkservers-count`, `metadataserver-status`)
	// already emit machine-parseable output without that flag.
	type cmdSpec struct {
		name string
		args []string
	}
	specs := []cmdSpec{
		{"info", []string{"info", porcelain}},
		{"list-chunkservers", []string{"list-chunkservers", porcelain}},
		{"list-metadataservers", []string{"list-metadataservers", porcelain}},
		{"metadataserver-status", []string{"metadataserver-status", porcelain}},
		{"chunks-health", []string{"chunks-health", porcelain}},
		{"list-disks", []string{"list-disks", porcelain}},
		{"list-goals", []string{"list-goals", porcelain}},
		{"list-mounts", []string{"list-mounts", porcelain}},
		{"ready-chunkservers-count", []string{"ready-chunkservers-count"}},
	}
	for i := range specs {
		specs[i].args = append(specs[i].args, tail...)
	}

	results := make(map[string]scrapeResult, len(specs))
	var wg sync.WaitGroup
	var resMu sync.Mutex
	for _, s := range specs {
		wg.Add(1)
		go func(s cmdSpec) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
			defer cancel()
			r := c.runCmd(ctx, s.args...)
			resMu.Lock()
			results[s.name] = r
			resMu.Unlock()
		}(s)
	}
	wg.Wait()

	// scrape-level metrics
	anyErr := false
	for name, r := range results {
		errVal := 0.0
		if r.err != nil {
			errVal = 1.0
			anyErr = true
			c.logger.Error(r.err, "saunafs-admin subcommand failed", "subcommand", name)
		}
		ch <- prometheus.MustNewConstMetric(c.scrapeErrorsDesc, prometheus.CounterValue, errVal, name)
		ch <- prometheus.MustNewConstMetric(c.scrapeDurationDesc, prometheus.GaugeValue, r.duration.Seconds(), name)
	}
	up := 1.0
	if anyErr {
		up = 0.0
	}
	ch <- prometheus.MustNewConstMetric(c.upDesc, prometheus.GaugeValue, up)

	// info
	if r, ok := results["info"]; ok && r.err == nil {
		if info, err := ParseInfo(r.out); err == nil {
			ch <- prometheus.MustNewConstMetric(c.infoDesc, prometheus.GaugeValue, 1, info.Version)
			ch <- prometheus.MustNewConstMetric(c.memoryDesc, prometheus.GaugeValue, float64(info.MemoryUsage))
			ch <- prometheus.MustNewConstMetric(c.spaceDesc, prometheus.GaugeValue, float64(info.TotalSpace), "total")
			ch <- prometheus.MustNewConstMetric(c.spaceDesc, prometheus.GaugeValue, float64(info.AvailableSpace), "available")
			ch <- prometheus.MustNewConstMetric(c.spaceDesc, prometheus.GaugeValue, float64(info.TrashSpace), "trash")
			ch <- prometheus.MustNewConstMetric(c.spaceDesc, prometheus.GaugeValue, float64(info.ReservedSpace), "reserved")
			ch <- prometheus.MustNewConstMetric(c.objectsDesc, prometheus.GaugeValue, float64(info.AllNodes), "all")
			ch <- prometheus.MustNewConstMetric(c.objectsDesc, prometheus.GaugeValue, float64(info.DirNodes), "dirs")
			ch <- prometheus.MustNewConstMetric(c.objectsDesc, prometheus.GaugeValue, float64(info.FileNodes), "files")
			ch <- prometheus.MustNewConstMetric(c.objectsDesc, prometheus.GaugeValue, float64(info.SymlinkNodes), "symlinks")
			ch <- prometheus.MustNewConstMetric(c.objectsDesc, prometheus.GaugeValue, float64(info.TrashNodes), "trash")
			ch <- prometheus.MustNewConstMetric(c.objectsDesc, prometheus.GaugeValue, float64(info.ReservedNodes), "reserved")
			ch <- prometheus.MustNewConstMetric(c.chunksTotalDesc, prometheus.GaugeValue, float64(info.Chunks))
			ch <- prometheus.MustNewConstMetric(c.chunkCopiesDesc, prometheus.GaugeValue, float64(info.ChunkCopies))
		} else {
			c.logger.Error(err, "parse info")
		}
	}

	// chunkservers
	if r, ok := results["list-chunkservers"]; ok && r.err == nil {
		if css, err := ParseChunkservers(r.out); err == nil {
			for _, cs := range css {
				disconnected := "no"
				if cs.Disconnected {
					disconnected = "yes"
				}
				ch <- prometheus.MustNewConstMetric(c.csInfoDesc, prometheus.GaugeValue, 1,
					cs.Address, cs.Label, cs.Version, disconnected)
				if cs.Disconnected {
					continue
				}
				ch <- prometheus.MustNewConstMetric(c.csChunksDesc, prometheus.GaugeValue, float64(cs.Chunks), cs.Address, cs.Label)
				ch <- prometheus.MustNewConstMetric(c.csUsedSpaceDesc, prometheus.GaugeValue, float64(cs.UsedSpace), cs.Address, cs.Label)
				ch <- prometheus.MustNewConstMetric(c.csTotalSpaceDesc, prometheus.GaugeValue, float64(cs.TotalSpace), cs.Address, cs.Label)
				ch <- prometheus.MustNewConstMetric(c.csErrorsDesc, prometheus.CounterValue, float64(cs.Errors), cs.Address, cs.Label)
				ch <- prometheus.MustNewConstMetric(c.csToDelChunks, prometheus.GaugeValue, float64(cs.ToDelChunks), cs.Address, cs.Label)
				ch <- prometheus.MustNewConstMetric(c.csToDelUsed, prometheus.GaugeValue, float64(cs.ToDelUsedSpace), cs.Address, cs.Label)
				ch <- prometheus.MustNewConstMetric(c.csToDelTotal, prometheus.GaugeValue, float64(cs.ToDelTotalSpace), cs.Address, cs.Label)
			}
		} else {
			c.logger.Error(err, "parse list-chunkservers")
		}
	}

	// ready-chunkservers-count
	if r, ok := results["ready-chunkservers-count"]; ok && r.err == nil {
		if v, err := ParseReadyChunkserversCount(r.out); err == nil {
			ch <- prometheus.MustNewConstMetric(c.readyCSDesc, prometheus.GaugeValue, float64(v))
		} else {
			c.logger.Error(err, "parse ready-chunkservers-count")
		}
	}

	// metadataservers
	if r, ok := results["list-metadataservers"]; ok && r.err == nil {
		if mss, err := ParseMetadataservers(r.out); err == nil {
			for _, ms := range mss {
				ch <- prometheus.MustNewConstMetric(c.msInfoDesc, prometheus.GaugeValue, 1,
					ms.IP, fmt.Sprintf("%d", ms.Port), ms.Hostname, ms.Personality, ms.ServerStatus, ms.Version)
				ch <- prometheus.MustNewConstMetric(c.msMetaVerDesc, prometheus.GaugeValue, float64(ms.MetadataVersion), ms.IP, ms.Personality)
				healthy := 0.0
				if (ms.Personality == "master" && ms.ServerStatus == "running") ||
					(ms.Personality == "shadow" && ms.ServerStatus == "connected") {
					healthy = 1.0
				}
				ch <- prometheus.MustNewConstMetric(c.msStatusDesc, prometheus.GaugeValue, healthy, ms.IP, ms.Personality)
			}
		} else {
			c.logger.Error(err, "parse list-metadataservers")
		}
	}

	// chunks-health
	if r, ok := results["chunks-health"]; ok && r.err == nil {
		if rep, err := ParseChunksHealth(r.out); err == nil {
			for _, a := range rep.Availability {
				ch <- prometheus.MustNewConstMetric(c.chAvaSafeDesc, prometheus.GaugeValue, float64(a.Safe), a.Goal)
				ch <- prometheus.MustNewConstMetric(c.chAvaEndangered, prometheus.GaugeValue, float64(a.Endangered), a.Goal)
				ch <- prometheus.MustNewConstMetric(c.chAvaLostDesc, prometheus.GaugeValue, float64(a.Lost), a.Goal)
			}
			for _, rp := range rep.Replication {
				for i, n := range rp.Counts {
					ch <- prometheus.MustNewConstMetric(c.chRepDesc, prometheus.GaugeValue, float64(n), rp.Goal, fmt.Sprintf("%d", i))
				}
			}
			for _, dl := range rep.Deletion {
				for i, n := range dl.Counts {
					ch <- prometheus.MustNewConstMetric(c.chDelDesc, prometheus.GaugeValue, float64(n), dl.Goal, fmt.Sprintf("%d", i))
				}
			}
		} else {
			c.logger.Error(err, "parse chunks-health")
		}
	}

	// list-disks
	if r, ok := results["list-disks"]; ok && r.err == nil {
		if disks, err := ParseDisks(r.out); err == nil {
			for _, d := range disks {
				ch <- prometheus.MustNewConstMetric(c.diskTotalDesc, prometheus.GaugeValue, float64(d.Total), d.ChunkserverAddress, d.Path)
				ch <- prometheus.MustNewConstMetric(c.diskUsedDesc, prometheus.GaugeValue, float64(d.Used), d.ChunkserverAddress, d.Path)
				ch <- prometheus.MustNewConstMetric(c.diskChunksDesc, prometheus.GaugeValue, float64(d.Chunks), d.ChunkserverAddress, d.Path)
				for flag, set := range map[string]bool{
					"to_delete": d.ToDelete,
					"damaged":   d.Damaged,
					"scanning":  d.Scanning,
				} {
					v := 0.0
					if set {
						v = 1.0
					}
					ch <- prometheus.MustNewConstMetric(c.diskFlagsDesc, prometheus.GaugeValue, v, d.ChunkserverAddress, d.Path, flag)
				}
			}
		} else {
			c.logger.Error(err, "parse list-disks")
		}
	}

	// goals
	if r, ok := results["list-goals"]; ok && r.err == nil {
		if goals, err := ParseGoals(r.out); err == nil {
			for _, g := range goals {
				ch <- prometheus.MustNewConstMetric(c.goalInfoDesc, prometheus.GaugeValue, 1,
					fmt.Sprintf("%d", g.ID), g.Name, g.Definition)
			}
		} else {
			c.logger.Error(err, "parse list-goals")
		}
	}

	// mounts
	if r, ok := results["list-mounts"]; ok && r.err == nil {
		if mounts, err := ParseMounts(r.out); err == nil {
			ch <- prometheus.MustNewConstMetric(c.mountsTotalDesc, prometheus.GaugeValue, float64(len(mounts)))
			for _, m := range mounts {
				ro := "no"
				if m.ReadOnly {
					ro = "yes"
				}
				ma := "no"
				if m.MapAll {
					ma = "yes"
				}
				ch <- prometheus.MustNewConstMetric(c.mountInfoDesc, prometheus.GaugeValue, 1,
					fmt.Sprintf("%d", m.SessionID), m.PeerIP, m.Info, m.Version, m.Path, ro, ma)
			}
		} else {
			c.logger.Error(err, "parse list-mounts")
		}
	}
}
