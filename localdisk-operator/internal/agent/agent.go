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

// Package agent implements the disk-agent logic that runs as a DaemonSet on
// every node. It is responsible for:
//
//  1. Scanning block devices via /sys/block and udev attributes.
//  2. Creating/updating LocalDisk CRs for each discovered device.
//  3. Formatting empty disks when spec.format=true (xfs by default, ext4 supported).
//  4. Bind-mounting formatted disks into the directory watched by
//     local-static-provisioner.
package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	diskv1alpha1 "github.com/henres/localdisk-operator/api/v1alpha1"
)

const (
	// defaultScanInterval is how often the agent rescans block devices.
	defaultScanInterval = 60 * time.Second

	// sysBlockPath is the sysfs directory containing block device entries.
	sysBlockPath = "/sys/block"

	// devPath is the directory where block device nodes live.
	devPath = "/dev"
)

// Agent scans block devices on the local node, manages LocalDisk CRs and
// performs disk formatting + mounting.
type Agent struct {
	client             client.Client
	nodeName           string
	mountBaseDir       string
	scanInterval       time.Duration
	includeLoopDevices bool
}

// Option configures the Agent.
type Option func(*Agent)

// WithIncludeLoopDevices makes the agent include loop devices in discovery.
// This is useful for Kind / test environments where loop devices simulate disks.
func WithIncludeLoopDevices(v bool) Option {
	return func(a *Agent) {
		a.includeLoopDevices = v
	}
}

// WithScanInterval overrides the default scan interval.
func WithScanInterval(d time.Duration) Option {
	return func(a *Agent) {
		if d > 0 {
			a.scanInterval = d
		}
	}
}

// New creates a new Agent.
// nodeName must match the Kubernetes node name (typically the hostname).
// mountBaseDir is the base directory for bind-mounts (e.g. /mnt/localdisk).
func New(c client.Client, nodeName, mountBaseDir string, opts ...Option) *Agent {
	a := &Agent{
		client:       c,
		nodeName:     nodeName,
		mountBaseDir: mountBaseDir,
		scanInterval: defaultScanInterval,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Run starts the agent scan loop. It blocks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	logger := ctrl.Log.WithName("disk-agent").WithValues("node", a.nodeName)
	logger.Info("Starting disk-agent", "mountBaseDir", a.mountBaseDir, "scanInterval", a.scanInterval)

	if err := os.MkdirAll(a.mountBaseDir, 0755); err != nil {
		return fmt.Errorf("creating mountBaseDir %s: %w", a.mountBaseDir, err)
	}

	ticker := time.NewTicker(a.scanInterval)
	defer ticker.Stop()

	// Run immediately on start, then on each tick.
	a.scan(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			a.scan(ctx)
		}
	}
}

// scan performs a single discovery + reconciliation pass.
func (a *Agent) scan(ctx context.Context) {
	logger := ctrl.Log.WithName("disk-agent").WithValues("node", a.nodeName)

	devices, err := discoverBlockDevices(a.includeLoopDevices)
	if err != nil {
		logger.Error(err, "Failed to discover block devices")
		return
	}

	seen := map[string]bool{}
	for _, dev := range devices {
		seen[dev.name] = true
		if err := a.reconcileDisk(ctx, dev); err != nil {
			logger.Error(err, "Failed to reconcile disk", "device", dev.name)
		}
	}

	// Mark disks that are no longer present as Missing.
	if err := a.markMissingDisks(ctx, seen); err != nil {
		logger.Error(err, "Failed to mark missing disks")
	}
}

// blockDevice holds raw information about a block device discovered via sysfs.
type blockDevice struct {
	name   string // e.g. "sdb"
	serial string
	size   int64 // bytes
	fsType string
	uuid   string
}

// discoverBlockDevices scans /sys/block and returns all candidate block
// devices with their attributes. Loop devices are included only when
// includeLoop is true.
func discoverBlockDevices(includeLoop bool) ([]blockDevice, error) {
	entries, err := os.ReadDir(sysBlockPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", sysBlockPath, err)
	}

	var devices []blockDevice
	for _, e := range entries {
		name := e.Name()
		if isVirtualDevice(name, includeLoop) {
			continue
		}
		dev, err := probeDevice(name)
		if err != nil {
			// Non-fatal: log and continue.
			ctrl.Log.WithName("disk-agent").Error(err, "Failed to probe device", "device", name)
			continue
		}
		devices = append(devices, dev)
	}
	return devices, nil
}

// isVirtualDevice returns true for devices we should never manage.
// When includeLoop is true, loop devices are not considered virtual.
func isVirtualDevice(name string, includeLoop bool) bool {
	prefixes := []string{"ram", "dm-", "md", "sr", "fd", "zram"}
	if !includeLoop {
		prefixes = append(prefixes, "loop")
	}
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// probeDevice reads sysfs and runs blkid to gather attributes for a device.
func probeDevice(name string) (blockDevice, error) {
	dev := blockDevice{name: name}

	// Size: /sys/block/<name>/size (in 512-byte sectors)
	sizeBytes, err := readSysfsInt(filepath.Join(sysBlockPath, name, "size"))
	if err == nil {
		dev.size = sizeBytes * 512
	}

	// Serial: /sys/block/<name>/device/serial (may not exist)
	serial, err := readSysfsString(filepath.Join(sysBlockPath, name, "device", "serial"))
	if err == nil {
		dev.serial = strings.TrimSpace(serial)
	}

	// Filesystem type and UUID via blkid (requires privileged).
	devPath := filepath.Join("/dev", name)
	fsType, uuid, err := blkid(devPath)
	if err == nil {
		dev.fsType = fsType
		dev.uuid = uuid
	}

	return dev, nil
}

// blkid runs blkid to detect filesystem type and UUID on a device.
// Returns empty strings (no error) if the device has no filesystem.
func blkid(devPath string) (fsType, uuid string, err error) {
	// --cache-file /dev/null prevents blkid from using stale cache.
	out, err := exec.Command("blkid", "--cache-file", "/dev/null",
		"-o", "export", devPath).Output()
	if err != nil {
		// Exit code 2 means no filesystem found — not an error for us.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 2 {
			return "", "", nil
		}
		return "", "", fmt.Errorf("blkid %s: %w", devPath, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			switch k {
			case "TYPE":
				fsType = v
			case "UUID":
				uuid = v
			}
		}
	}
	return fsType, uuid, nil
}

// reconcileDisk ensures a LocalDisk CR exists for the given device and
// processes any pending formatting requests.
func (a *Agent) reconcileDisk(ctx context.Context, dev blockDevice) error {
	logger := ctrl.Log.WithName("disk-agent").WithValues("node", a.nodeName, "device", dev.name)

	crName := fmt.Sprintf("%s-%s", dev.name, a.nodeName)
	disk := &diskv1alpha1.LocalDisk{}

	err := a.client.Get(ctx, types.NamespacedName{Name: crName}, disk)
	if errors.IsNotFound(err) {
		disk = a.newLocalDisk(crName, dev)
		if createErr := a.client.Create(ctx, disk); createErr != nil {
			return fmt.Errorf("creating LocalDisk %s: %w", crName, createErr)
		}
		// Status subresource is not set by Create — patch it immediately.
		updated := disk.DeepCopy()
		updated.Status = a.initialStatus(dev)
		if patchErr := a.patchStatus(ctx, disk, updated); patchErr != nil {
			return fmt.Errorf("patching initial status for %s: %w", crName, patchErr)
		}
		logger.Info("Created LocalDisk CR", "cr", crName)
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting LocalDisk %s: %w", crName, err)
	}

	// Update status to reflect current observations.
	updated := disk.DeepCopy()
	updated.Status.Node = a.nodeName
	updated.Status.Device = "/dev/" + dev.name
	updated.Status.Serial = dev.serial
	updated.Status.SizeBytes = dev.size
	updated.Status.SizeHumanReadable = humanBytes(dev.size)
	updated.Status.FSType = dev.fsType
	updated.Status.UUID = dev.uuid
	updated.Status.LastUpdated = &metav1.Time{Time: time.Now()}

	// Determine state transitions.
	switch {
	case dev.fsType != "" && disk.Status.State != diskv1alpha1.LocalDiskStateReady:
		// Disk has a filesystem but wasn't formatted by us.
		if disk.Status.State == diskv1alpha1.LocalDiskStateFormatting {
			// We were formatting — it's done.
			updated.Status.State = diskv1alpha1.LocalDiskStateReady
			updated.Status.Message = ""
			logger.Info("Disk formatting complete", "uuid", dev.uuid)
			if err := a.mountDisk(dev); err != nil {
				updated.Status.State = diskv1alpha1.LocalDiskStateFormatFailed
				updated.Status.Message = err.Error()
			} else {
				updated.Status.MountPath = filepath.Join(a.mountBaseDir, dev.uuid)
			}
		} else if disk.Status.State == diskv1alpha1.LocalDiskStateEmpty ||
			disk.Status.State == diskv1alpha1.LocalDiskStatePendingFormat {
			// Filesystem existed before we touched it.
			updated.Status.State = diskv1alpha1.LocalDiskStateUnmanaged
			updated.Status.Message = "Disk has an existing filesystem; will not be modified"
		}

	case dev.fsType == "" && disk.Spec.Format &&
		(disk.Status.State == diskv1alpha1.LocalDiskStateEmpty ||
			disk.Status.State == diskv1alpha1.LocalDiskStatePendingFormat ||
			disk.Status.State == diskv1alpha1.LocalDiskStateFormatFailed):
		// Format requested on an empty disk — start formatting.
		fsType := disk.Spec.FSType
		if fsType == "" {
			fsType = "xfs"
		}
		updated.Status.State = diskv1alpha1.LocalDiskStateFormatting
		updated.Status.Message = fmt.Sprintf("Formatting with %s in progress", fsType)
		if patchErr := a.patchStatus(ctx, disk, updated); patchErr != nil {
			return patchErr
		}
		if err := a.formatDisk(dev, fsType); err != nil {
			updated.Status.State = diskv1alpha1.LocalDiskStateFormatFailed
			updated.Status.Message = err.Error()
			logger.Error(err, "Disk formatting failed")
		} else {
			// Re-probe to get UUID.
			detectedFsType, uuid, _ := blkid("/dev/" + dev.name)
			updated.Status.UUID = uuid
			updated.Status.FSType = detectedFsType
			updated.Status.State = diskv1alpha1.LocalDiskStateReady
			updated.Status.Message = ""
			if err := a.mountDisk(blockDevice{name: dev.name, uuid: uuid, size: dev.size}); err != nil {
				updated.Status.State = diskv1alpha1.LocalDiskStateFormatFailed
				updated.Status.Message = fmt.Sprintf("mount failed: %v", err)
			} else {
				updated.Status.MountPath = filepath.Join(a.mountBaseDir, uuid)
			}
			logger.Info("Disk formatted and mounted", "fsType", detectedFsType, "uuid", uuid)
		}

	case dev.fsType == "" && disk.Status.State == diskv1alpha1.LocalDiskStateEmpty:
		updated.Status.State = diskv1alpha1.LocalDiskStateEmpty

	case dev.fsType == "" && !disk.Spec.Format &&
		disk.Status.State != diskv1alpha1.LocalDiskStateEmpty:
		updated.Status.State = diskv1alpha1.LocalDiskStateEmpty
	}

	return a.patchStatus(ctx, disk, updated)
}

// newLocalDisk creates a new LocalDisk CR for a freshly discovered device.
// Note: the Status field is ignored by Create when the CRD uses a status
// subresource. Call patchStatus immediately after Create.
func (a *Agent) newLocalDisk(name string, dev blockDevice) *diskv1alpha1.LocalDisk {
	return &diskv1alpha1.LocalDisk{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"localdisk-operator.io/node": a.nodeName,
			},
		},
	}
}

// initialStatus builds the status for a freshly discovered device.
func (a *Agent) initialStatus(dev blockDevice) diskv1alpha1.LocalDiskStatus {
	state := diskv1alpha1.LocalDiskStateEmpty
	if dev.fsType != "" {
		state = diskv1alpha1.LocalDiskStateUnmanaged
	}
	return diskv1alpha1.LocalDiskStatus{
		Node:              a.nodeName,
		Device:            "/dev/" + dev.name,
		Serial:            dev.serial,
		SizeBytes:         dev.size,
		SizeHumanReadable: humanBytes(dev.size),
		FSType:            dev.fsType,
		UUID:              dev.uuid,
		State:             state,
		LastUpdated:       &metav1.Time{Time: time.Now()},
	}
}

// markMissingDisks sets State=Missing for CRs whose device is no longer seen.
func (a *Agent) markMissingDisks(ctx context.Context, seen map[string]bool) error {
	list := &diskv1alpha1.LocalDiskList{}
	if err := a.client.List(ctx, list,
		client.MatchingLabels{"localdisk-operator.io/node": a.nodeName}); err != nil {
		return err
	}
	for i := range list.Items {
		disk := &list.Items[i]
		devName := strings.TrimPrefix(disk.Status.Device, "/dev/")
		if !seen[devName] && disk.Status.State != diskv1alpha1.LocalDiskStateMissing {
			updated := disk.DeepCopy()
			updated.Status.State = diskv1alpha1.LocalDiskStateMissing
			updated.Status.Message = "Device no longer detected on node"
			updated.Status.LastUpdated = &metav1.Time{Time: time.Now()}
			if err := a.patchStatus(ctx, disk, updated); err != nil {
				ctrl.Log.WithName("disk-agent").Error(err, "Failed to mark disk missing", "cr", disk.Name)
			}
		}
	}
	return nil
}

// formatDisk runs mkfs with the specified filesystem type on the given device.
// Supported types: xfs, ext4. This is destructive and irreversible.
func (a *Agent) formatDisk(dev blockDevice, fsType string) error {
	devPath := "/dev/" + dev.name
	mkfs := "mkfs." + fsType
	var args []string
	switch fsType {
	case "xfs":
		args = []string{"-f", devPath}
	case "ext4":
		args = []string{"-F", devPath}
	default:
		return fmt.Errorf("unsupported filesystem type: %s", fsType)
	}
	cmd := exec.Command(mkfs, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w\n%s", mkfs, devPath, err, string(out))
	}
	return nil
}

// mountDisk bind-mounts the formatted device into the localdisk mount base dir.
// The mount point is <mountBaseDir>/<uuid>.
func (a *Agent) mountDisk(dev blockDevice) error {
	mountPoint := filepath.Join(a.mountBaseDir, dev.uuid)
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", mountPoint, err)
	}
	devPath := "/dev/" + dev.name
	cmd := exec.Command("mount", devPath, mountPoint)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount %s → %s: %w\n%s", devPath, mountPoint, err, string(out))
	}
	return nil
}

// patchStatus applies a status subresource patch.
func (a *Agent) patchStatus(ctx context.Context, original, updated *diskv1alpha1.LocalDisk) error {
	return a.client.Status().Patch(ctx, updated,
		client.MergeFrom(original))
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func readSysfsString(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func readSysfsInt(path string) (int64, error) {
	s, err := readSysfsString(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(s, 10, 64)
}

// humanBytes formats bytes into a human-readable string (GiB/TiB).
func humanBytes(b int64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
		tib = 1024 * gib
	)
	switch {
	case b >= tib:
		return fmt.Sprintf("%.2f TiB", float64(b)/float64(tib))
	case b >= gib:
		return fmt.Sprintf("%.2f GiB", float64(b)/float64(gib))
	case b >= mib:
		return fmt.Sprintf("%.2f MiB", float64(b)/float64(mib))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
