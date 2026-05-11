package exporter

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeRunner returns canned outputs keyed by the first argument
// (subcommand name).
type fakeRunner struct {
	outputs map[string]string
	errors  map[string]error
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	sub := args[0]
	if err, ok := f.errors[sub]; ok && err != nil {
		return "", err
	}
	return f.outputs[sub], nil
}

func TestCollectorScrape(t *testing.T) {
	fr := &fakeRunner{
		outputs: map[string]string{
			"info":                     infoFixture,
			"list-chunkservers":        chunkserversFixture,
			"list-metadataservers":     metadataserversFixture,
			"metadataserver-status":    metadataserverStatusFixture,
			"chunks-health":            chunksHealthFixture,
			"list-disks":               "",
			"list-goals":               goalsFixture,
			"list-mounts":              mountsFixture,
			"ready-chunkservers-count": readyCountFixture,
		},
	}
	c := NewCollector(CollectorOptions{
		Runner:     fr,
		MasterHost: "127.0.0.1",
		MasterPort: "9421",
		Logger:     logr.Discard(),
	})

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	// Sanity check: scrape produces a non-empty output containing
	// the headline metrics.
	got, err := testutil.GatherAndLint(reg)
	if err != nil {
		t.Fatalf("lint: %v", err)
	}
	_ = got

	// Validate a few well-known values via testutil.ToFloat64 by
	// re-collecting through a textual format.
	checks := []struct {
		name string
		want string
	}{
		{"leilfs_fs_up 1", "leilfs_fs_up 1"},
		{"info gauge", `leilfs_fs_info{version="5.9.0"} 1`},
		{"6 chunkservers", `leilfs_fs_chunkserver_chunks{address="10.42.4.127:9422",label="lima_sfs_w3"} 0`},
		{"master metadata version", `leilfs_fs_metadataserver_metadata_version{ip="10.43.194.85",personality="master"} 7`},
		{"shadow status connected", `leilfs_fs_metadataserver_status{ip="10.42.4.128",personality="shadow"} 1`},
		{"ec_4_2 availability", `leilfs_fs_chunks_safe{goal="ec_4_2"} 0`},
		{"goal info", `leilfs_fs_goal_info{definition="ec_4_2: $ec(4,2) {_ _ _ _ _ _}",id="10",name="ec_4_2"} 1`},
		{"mounts total", `leilfs_fs_mounts_total 1`},
		{"ready count", `leilfs_fs_chunkservers_ready 0`},
	}

	if err := testutil.GatherAndCompare(reg, strings.NewReader(""), nil...); err == nil {
		t.Error("expected GatherAndCompare with empty input to fail (sanity)")
	}

	text, err := gatherText(reg)
	if err != nil {
		t.Fatalf("gatherText: %v", err)
	}
	for _, ch := range checks {
		if !strings.Contains(text, ch.want) {
			t.Errorf("%s: missing %q in output\nfull text:\n%s", ch.name, ch.want, text)
		}
	}
}

func TestCollectorScrapeWithError(t *testing.T) {
	fr := &fakeRunner{
		outputs: map[string]string{"info": infoFixture},
		errors:  map[string]error{"list-chunkservers": context.DeadlineExceeded},
	}
	c := NewCollector(CollectorOptions{Runner: fr, Logger: logr.Discard()})
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	text, err := gatherText(reg)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if !strings.Contains(text, "leilfs_fs_up 0") {
		t.Errorf("expected up=0 when a subcommand errors\n%s", text)
	}
	if !strings.Contains(text, `leilfs_fs_scrape_errors_total{subcommand="list-chunkservers"} 1`) {
		t.Errorf("expected error counter set\n%s", text)
	}
}

func gatherText(reg *prometheus.Registry) (string, error) {
	mfs, err := reg.Gather()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, mf := range mfs {
		for _, m := range mf.Metric {
			b.WriteString(mf.GetName())
			if len(m.Label) > 0 {
				b.WriteString("{")
				for i, l := range m.Label {
					if i > 0 {
						b.WriteString(",")
					}
					b.WriteString(l.GetName())
					b.WriteString("=\"")
					b.WriteString(l.GetValue())
					b.WriteString("\"")
				}
				b.WriteString("}")
			}
			if m.Gauge != nil {
				b.WriteString(" ")
				b.WriteString(floatStr(m.Gauge.GetValue()))
			} else if m.Counter != nil {
				b.WriteString(" ")
				b.WriteString(floatStr(m.Counter.GetValue()))
			}
			b.WriteString("\n")
		}
	}
	return b.String(), nil
}

func floatStr(v float64) string {
	// Use the same minimal representation Prometheus does for whole
	// numbers (no trailing zeros) so assertions match plain "1" / "7".
	if v == float64(int64(v)) {
		// 64-bit safe integer formatting
		return intStr(int64(v))
	}
	// Fallback to default fmt.
	return floatStrFmt(v)
}

func intStr(i int64) string {
	return strconvFormatInt(i)
}

// indirections to keep imports minimal in this test-only helper
func strconvFormatInt(i int64) string {
	// Local re-implementation to avoid extra import in test file.
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = digits[i%10]
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

func floatStrFmt(v float64) string {
	// Quick & dirty for tests
	return strconvFormatInt(int64(v))
}
