package baseline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cacggghp/vk-turn-proxy/metrics"
)

type fakeIPerf struct {
	calls  int
	failAt int
}

func (f *fakeIPerf) Version(context.Context) (string, error) {
	return "iperf 3.test", nil
}

func (f *fakeIPerf) Run(_ context.Context, _ string, _ int, benchmarkCase Case) (CommandResult, error) {
	f.calls++
	reverse := 0
	if benchmarkCase.Direction == DirectionDownload {
		reverse = 1
	}
	rate := float64(100_000_000 + f.calls*1_000_000)
	raw := fmt.Sprintf(`{"start":{"version":"iperf 3.test","system_info":"fake","test_start":{"protocol":"TCP","num_streams":%d,"reverse":%d}},"end":{"sum_sent":{"seconds":1,"bytes":100,"bits_per_second":%.0f,"retransmits":%d},"sum_received":{"seconds":1,"bytes":99,"bits_per_second":%.0f}}}`,
		benchmarkCase.Parallel, reverse, rate, f.calls, rate-1000)
	result := CommandResult{Arguments: []string{"fake", benchmarkCase.Slug()}, Stdout: []byte(raw), Stderr: []byte("fake-stderr")}
	if f.failAt == f.calls {
		return result, fmt.Errorf("injected iperf failure")
	}
	return result, nil
}

type fakeMetrics struct {
	fetches uint64
}

func (f *fakeMetrics) Fetch(context.Context) (metrics.Snapshot, error) {
	f.fetches++
	return metrics.Snapshot{
		ActiveSessions: 1,
		ActivePaths:    2,
		BytesRead:      f.fetches * 100,
		BytesWritten:   f.fetches * 200,
		KCP: metrics.KCPSnapshot{
			RetransmittedSegments: f.fetches * 3,
			FECRecoveredPackets:   f.fetches * 4,
		},
	}, nil
}

func TestHarnessWritesCompleteReproducibleReport(t *testing.T) {
	output := filepath.Join(t.TempDir(), "result")
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	ipref := &fakeIPerf{}
	metricSource := &fakeMetrics{}
	harness := Harness{
		IPerf: ipref, Metrics: metricSource, Now: clock,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	config := RunConfig{
		Scenario:        Scenario{Name: "turn-tcp-bond-n2", TURNTransport: "tcp", ProxyMode: "bond", Sessions: 2},
		Revision:        "deadbeef",
		Target:          "127.0.0.1:9000",
		IPerfExecutable: "iperf3",
		DiagnosticsURL:  "http://127.0.0.1:6060",
		OutputDirectory: output,
		Runs:            3,
		WarmupRuns:      1,
		DurationSeconds: 1,
		Settle:          time.Millisecond,
		Cooldown:        time.Millisecond,
		Cases:           []Case{{Direction: DirectionUpload, Parallel: 1}},
		Notes:           "test run",
	}
	report, err := harness.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Status != "complete" || len(report.Warmups) != 1 || len(report.Runs) != 3 || len(report.Summaries) != 1 {
		t.Fatalf("report = %+v", report)
	}
	if ipref.calls != 4 || metricSource.fetches != 7 {
		t.Fatalf("calls = iperf:%d metrics:%d, want 4 and 7", ipref.calls, metricSource.fetches)
	}
	for _, run := range report.Runs {
		if run.MetricsDelta.BytesRead != 100 || run.MetricsDelta.BytesWritten != 200 ||
			run.MetricsDelta.KCP.RetransmittedSegments != 3 || run.MetricsDelta.KCP.FECRecoveredPackets != 4 {
			t.Fatalf("run delta = %+v", run.MetricsDelta)
		}
		for _, name := range []string{run.Files.IPerfJSON, run.Files.IPerfStderr, run.Files.MetricsBefore, run.Files.MetricsAfter} {
			if _, err := os.Stat(filepath.Join(output, name)); err != nil {
				t.Fatalf("artifact %s: %v", name, err)
			}
		}
	}

	reportData, err := os.ReadFile(filepath.Join(output, "report.json"))
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var persisted Report
	if err := json.Unmarshal(reportData, &persisted); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if persisted.Status != "complete" || persisted.FinishedAt == nil || persisted.Revision != "deadbeef" || persisted.IPerfVersion != "iperf 3.test" {
		t.Fatalf("persisted report = %+v", persisted)
	}
}

func TestRunConfigRejectsExistingOutputAndDuplicateCases(t *testing.T) {
	base := RunConfig{
		Scenario: Scenario{Name: "test", TURNTransport: "udp", ProxyMode: "multi-session", Sessions: 1},
		Revision: "rev", Target: "127.0.0.1:9000", IPerfExecutable: "iperf3",
		DiagnosticsURL: "http://127.0.0.1:6060", OutputDirectory: "result",
		Runs: 3, DurationSeconds: 1, Cases: []Case{{Direction: DirectionUpload, Parallel: 1}},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	base.Cases = append(base.Cases, base.Cases[0])
	if err := base.Validate(); err == nil {
		t.Fatal("duplicate case accepted")
	}

	output := t.TempDir()
	harness := Harness{IPerf: &fakeIPerf{}, Metrics: &fakeMetrics{}}
	base.Cases = base.Cases[:1]
	base.OutputDirectory = output
	if _, err := harness.Run(context.Background(), base); err == nil {
		t.Fatal("existing output directory accepted")
	}
}

func TestHarnessPersistsFailureAndRawArtifacts(t *testing.T) {
	output := filepath.Join(t.TempDir(), "failed-result")
	harness := Harness{IPerf: &fakeIPerf{failAt: 1}, Metrics: &fakeMetrics{}}
	config := RunConfig{
		Scenario: Scenario{Name: "failed-test", TURNTransport: "tcp", ProxyMode: "multi-session", Sessions: 1},
		Revision: "deadbeef", Target: "127.0.0.1:9000", IPerfExecutable: "iperf3",
		DiagnosticsURL: "http://127.0.0.1:6060", OutputDirectory: output,
		Runs: 3, DurationSeconds: 1, Cases: []Case{{Direction: DirectionUpload, Parallel: 1}},
	}
	report, err := harness.Run(context.Background(), config)
	if err == nil || report == nil || report.Status != "failed" || report.FinishedAt == nil {
		t.Fatalf("failed Run = report:%+v error:%v", report, err)
	}
	for _, name := range []string{
		"run-upload-p1-01.iperf.json",
		"run-upload-p1-01.iperf.stderr.txt",
		"run-upload-p1-01.metrics-before.json",
		"run-upload-p1-01.metrics-after.json",
		"report.json",
	} {
		if _, statErr := os.Stat(filepath.Join(output, name)); statErr != nil {
			t.Fatalf("failure artifact %s: %v", name, statErr)
		}
	}
}

func TestValidateIPerfCase(t *testing.T) {
	benchmarkCase := Case{Direction: DirectionDownload, Parallel: 8}
	valid := IPerfSummary{Protocol: "TCP", RequestedStreams: 8, Reverse: true}
	if err := validateIPerfCase(benchmarkCase, valid); err != nil {
		t.Fatalf("valid iperf summary rejected: %v", err)
	}
	valid.Reverse = false
	if err := validateIPerfCase(benchmarkCase, valid); err == nil {
		t.Fatal("wrong reverse flag accepted")
	}
}
