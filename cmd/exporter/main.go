// Command leilfs-exporter is a Prometheus exporter for LeilFS clusters.
//
// It executes `saunafs-admin <subcommand> --porcelain` against an
// sfsmaster (by default localhost:9421, as expected when running as a
// sidecar on a master Pod) and exposes the parsed results as
// `leilfs_fs_*` metrics on /metrics.
//
// The exporter is intentionally minimal: no persistence, no leader
// election, no cluster-internal state. Failover is handled by
// Kubernetes: when the active master Pod restarts, the sidecar
// restarts with it, and its previously-emitted series simply
// disappear from Prometheus on the next scrape of the new active
// master Pod.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/henres/leilfs-operator/internal/exporter"
)

func main() {
	var (
		listen     = flag.String("listen-address", ":9418", "TCP address to listen on for /metrics scrapes.")
		masterHost = flag.String("master-host", "127.0.0.1", "Hostname or IP of the sfsmaster admin endpoint.")
		masterPort = flag.String("master-port", "9421", "TCP port of the sfsmaster client/admin endpoint.")
		timeout    = flag.Duration("scrape-timeout", 3*time.Second, "Per-subcommand saunafs-admin invocation timeout.")
		adminBin   = flag.String("admin-binary", "saunafs-admin", "Path to the saunafs-admin binary.")
		showVer    = flag.Bool("version", false, "Print version and exit.")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("leilfs-exporter")
		return
	}

	log := newLogger()

	collector := exporter.NewCollector(exporter.CollectorOptions{
		Runner:     &exporter.ExecRunner{Binary: *adminBin},
		MasterHost: *masterHost,
		MasterPort: *masterPort,
		Timeout:    *timeout,
		Logger:     log,
	})

	reg := prometheus.NewRegistry()
	reg.MustRegister(collector)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		Registry:          reg,
		EnableOpenMetrics: true,
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>
<head><title>LeilFS Exporter</title></head>
<body>
<h1>LeilFS Exporter</h1>
<p><a href="/metrics">Metrics</a></p>
</body>
</html>`))
	})

	log.Info("starting leilfs-exporter",
		"listen", *listen,
		"master", fmt.Sprintf("%s:%s", *masterHost, *masterPort),
		"timeout", timeout.String())

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Error(err, "http server terminated")
		os.Exit(1)
	}
}

// newLogger returns a logr.Logger backed by zap with sane production
// defaults (ISO timestamps, JSON output).
func newLogger() logr.Logger {
	cfg := zap.NewProductionConfig()
	cfg.EncoderConfig.TimeKey = "ts"
	cfg.EncoderConfig.EncodeTime = zapcore.RFC3339TimeEncoder
	zl, err := cfg.Build()
	if err != nil {
		panic(err)
	}
	return zapr.NewLogger(zl)
}
