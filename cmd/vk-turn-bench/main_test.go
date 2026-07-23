package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/l1ch666/vk-turn-proxy/v2/baseline"
)

func TestParseCLIProducesDeterministicMatrix(t *testing.T) {
	now := time.Date(2026, 7, 12, 15, 4, 5, 123, time.UTC)
	options, err := parseCLI([]string{
		"-scenario", "turn-udp-bond-n10",
		"-turn-transport", "udp",
		"-proxy-mode", "bond",
		"-sessions", "10",
		"-target", "127.0.0.1:9001",
		"-runs", "5",
		"-warmups", "2",
		"-duration", "30s",
		"-settle", "1500ms",
		"-cooldown", "3s",
		"-flows", "1,4",
		"-directions", "download,upload",
		"-diagnostics-url", "http://127.0.0.1:7000",
		"-output", "results",
		"-revision", "abc123",
		"-dry-run",
	}, &bytes.Buffer{}, func() time.Time { return now }, func() string { return "ignored" })
	if err != nil {
		t.Fatalf("parseCLI: %v", err)
	}
	config := options.RunConfig
	wantCases := []baseline.Case{
		{Direction: baseline.DirectionDownload, Parallel: 1},
		{Direction: baseline.DirectionDownload, Parallel: 4},
		{Direction: baseline.DirectionUpload, Parallel: 1},
		{Direction: baseline.DirectionUpload, Parallel: 4},
	}
	if !options.DryRun || config.Scenario.TURNTransport != "udp" || config.Scenario.ProxyMode != "bond" ||
		config.Runs != 5 || config.WarmupRuns != 2 || config.DurationSeconds != 30 ||
		config.Settle != 1500*time.Millisecond || config.Cooldown != 3*time.Second ||
		len(config.Cases) != len(wantCases) {
		t.Fatalf("parsed config = %+v", config)
	}
	for i := range wantCases {
		if config.Cases[i] != wantCases[i] {
			t.Fatalf("case %d = %+v, want %+v", i, config.Cases[i], wantCases[i])
		}
	}
	if !strings.Contains(config.OutputDirectory, "turn-udp-bond-n10-20260712T150405.000000123Z") {
		t.Fatalf("output directory = %q", config.OutputDirectory)
	}
}

func TestRunDryRunNeedsNoTokenOrIPerf(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"-scenario=turn-tcp-multi-n1",
		"-turn-transport=tcp",
		"-proxy-mode=multi-session",
		"-sessions=1",
		"-duration=1s",
		"-cooldown=0",
		"-settle=0",
		"-revision=test-revision",
		"-dry-run",
	}, &stdout, &stderr, func() time.Time { return time.Unix(0, 0) }, func() string { return "" })
	if err != nil {
		t.Fatalf("dry run: %v; stderr=%s", err, stderr.String())
	}
	var plan dryRunPlan
	if err := json.Unmarshal(stdout.Bytes(), &plan); err != nil {
		t.Fatalf("decode dry-run plan: %v", err)
	}
	if plan.Revision != "test-revision" || len(plan.Cases) != 4 || plan.EstimatedDuration != "16s" {
		t.Fatalf("dry-run plan = %+v", plan)
	}
}

func TestParseCLIRejectsInvalidMatrices(t *testing.T) {
	base := []string{
		"-scenario=test",
		"-turn-transport=tcp",
		"-proxy-mode=bond",
		"-sessions=2",
		"-revision=rev",
	}
	for _, extra := range [][]string{
		{"-duration=1500ms"},
		{"-flows=1,1"},
		{"-directions=upload,upload"},
		{"-directions=sideways"},
		{"-output="},
	} {
		args := append(append([]string(nil), base...), extra...)
		if _, err := parseCLI(args, &bytes.Buffer{}, time.Now, func() string { return "" }); err == nil {
			t.Errorf("invalid args accepted: %v", extra)
		}
	}
}

func TestParseCLIAutoDetectsRevision(t *testing.T) {
	options, err := parseCLI([]string{
		"-scenario=test",
		"-turn-transport=tcp",
		"-proxy-mode=multi-session",
		"-sessions=1",
	}, &bytes.Buffer{}, time.Now, func() string { return "detected-revision" })
	if err != nil {
		t.Fatalf("parseCLI: %v", err)
	}
	if options.RunConfig.Revision != "detected-revision" {
		t.Fatalf("revision = %q", options.RunConfig.Revision)
	}
}
