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

// disk-agent runs as a DaemonSet pod on each Kubernetes node.
// It discovers block devices, manages LocalDisk CRs, formats disks on request
// and bind-mounts them for local-static-provisioner to discover.
package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	diskv1alpha1 "github.com/henres/localdisk-operator/api/v1alpha1"
	"github.com/henres/localdisk-operator/internal/agent"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(diskv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		nodeName           string
		mountBaseDir       string
		includeLoopDevices bool
		scanInterval       time.Duration
	)
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"),
		"Kubernetes node name (defaults to NODE_NAME env var)")
	flag.StringVar(&mountBaseDir, "mount-base-dir", "/mnt/localdisk",
		"Base directory for disk bind-mounts")
	flag.BoolVar(&includeLoopDevices, "include-loop-devices", false,
		"Include loop devices in discovery (useful for Kind/test environments)")
	flag.DurationVar(&scanInterval, "scan-interval", 60*time.Second,
		"How often the agent rescans block devices")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	logger := ctrl.Log.WithName("disk-agent")

	if nodeName == "" {
		logger.Error(nil, "node-name is required (set NODE_NAME env var or --node-name flag)")
		os.Exit(1)
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		logger.Error(err, "Failed to get kubeconfig")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		// Agent does not run an HTTP health server or webhook server.
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		logger.Error(err, "Failed to create manager")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	a := agent.New(mgr.GetClient(), nodeName, mountBaseDir,
		agent.WithIncludeLoopDevices(includeLoopDevices),
		agent.WithScanInterval(scanInterval),
	)

	// Start the manager (runs the cache/informers) then the agent loop.
	go func() {
		if err := mgr.Start(ctx); err != nil {
			logger.Error(err, "Manager exited")
			os.Exit(1)
		}
	}()

	// Wait for cache sync before first scan.
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		logger.Error(nil, "Cache sync timed out")
		os.Exit(1)
	}

	if err := a.Run(ctx); err != nil {
		logger.Error(err, "Agent exited with error")
		os.Exit(1)
	}
}
