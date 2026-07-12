package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/l1ch666/vk-turn-proxy/baseline"
	"github.com/l1ch666/vk-turn-proxy/diagnostics"
)

type cliOptions struct {
	RunConfig baseline.RunConfig
	TokenFile string
	DryRun    bool
}

type dryRunPlan struct {
	Scenario          baseline.Scenario `json:"scenario"`
	Revision          string            `json:"revision"`
	Target            string            `json:"target"`
	IPerfExecutable   string            `json:"iperf_executable"`
	DiagnosticsURL    string            `json:"diagnostics_url"`
	OutputDirectory   string            `json:"output_directory"`
	Runs              int               `json:"runs"`
	WarmupRuns        int               `json:"warmup_runs"`
	Duration          string            `json:"duration"`
	Settle            string            `json:"settle"`
	Cooldown          string            `json:"cooldown"`
	Cases             []baseline.Case   `json:"cases"`
	EstimatedDuration string            `json:"estimated_duration"`
	Notes             string            `json:"notes,omitempty"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr, time.Now, detectBuildRevision)
	stop()
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "vk-turn-bench: %v\n", err)
		os.Exit(1)
	}
}

func run(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	now func() time.Time,
	revisionDetector func() string,
) error {
	options, err := parseCLI(args, stderr, now, revisionDetector)
	if err != nil {
		return err
	}
	if options.DryRun {
		plan := makeDryRunPlan(options.RunConfig)
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(plan)
	}

	tokenConfig, err := (diagnostics.Options{
		ListenAddress: "127.0.0.1:1",
		TokenFile:     options.TokenFile,
	}).Config()
	if err != nil {
		return fmt.Errorf("load diagnostics token: %w", err)
	}
	metricsClient, err := baseline.NewHTTPMetricsClient(options.RunConfig.DiagnosticsURL, tokenConfig.BearerToken)
	if err != nil {
		return err
	}
	harness := baseline.Harness{
		IPerf:   baseline.CommandIPerf{Path: options.RunConfig.IPerfExecutable},
		Metrics: metricsClient,
	}
	report, err := harness.Run(ctx, options.RunConfig)
	if err != nil {
		if report != nil {
			return fmt.Errorf("baseline failed; partial artifacts: %s: %w", options.RunConfig.OutputDirectory, err)
		}
		return err
	}

	fmt.Fprintf(stdout, "Baseline complete: %s\n", options.RunConfig.OutputDirectory)
	for _, summary := range report.Summaries {
		fmt.Fprintf(stdout, "  %s: median %.2f Mbit/s, retransmits %.1f (%d runs)\n",
			summary.Case.Slug(),
			summary.MedianBitsPerSecond/1_000_000,
			summary.MedianRetransmittedSegments,
			summary.Runs,
		)
	}
	return nil
}

func parseCLI(
	args []string,
	stderr io.Writer,
	now func() time.Time,
	revisionDetector func() string,
) (cliOptions, error) {
	fs := flag.NewFlagSet("vk-turn-bench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	scenarioName := fs.String("scenario", "", "stable scenario name used in the output directory")
	turnTransport := fs.String("turn-transport", "", "TURN connection transport used by the proxy: tcp or udp")
	proxyMode := fs.String("proxy-mode", "", "proxy mode: multi-session or bond")
	sessions := fs.Int("sessions", 0, "configured proxy session/path count")
	target := fs.String("target", "127.0.0.1:9000", "local VLESS proxy address for iperf3")
	runs := fs.Int("runs", baseline.MinimumRuns, "measured runs per matrix case (minimum 3)")
	warmups := fs.Int("warmups", 1, "warmup runs per matrix case")
	duration := fs.Duration("duration", 20*time.Second, "iperf3 duration per run (whole seconds, maximum 5m)")
	settle := fs.Duration("settle", time.Second, "delay before the post-run diagnostics snapshot")
	cooldown := fs.Duration("cooldown", 2*time.Second, "delay between matrix operations")
	flows := fs.String("flows", "1,8", "comma-separated iperf3 parallel flow counts")
	directions := fs.String("directions", "upload,download", "comma-separated directions: upload,download")
	iperfExecutable := fs.String("iperf3", "iperf3", "iperf3 executable path")
	diagnosticsURL := fs.String("diagnostics-url", "http://127.0.0.1:6060", "authenticated client diagnostics origin")
	tokenFile := fs.String("diagnostics-token-file", "", "diagnostics token file; environment is used when empty")
	outputRoot := fs.String("output", "benchmarks/results", "benchmark result root directory")
	revision := fs.String("revision", "", "source revision; auto-detected from build metadata when empty")
	notes := fs.String("notes", "", "non-secret environment notes recorded in the report")
	dryRun := fs.Bool("dry-run", false, "validate and print the matrix without running iperf3")
	if err := fs.Parse(args); err != nil {
		return cliOptions{}, err
	}
	if fs.NArg() != 0 {
		return cliOptions{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	if now == nil || revisionDetector == nil {
		return cliOptions{}, fmt.Errorf("CLI dependencies must not be nil")
	}
	if *duration%time.Second != 0 {
		return cliOptions{}, fmt.Errorf("-duration must be a whole number of seconds")
	}
	flowCounts, err := parseFlows(*flows)
	if err != nil {
		return cliOptions{}, err
	}
	directionValues, err := parseDirections(*directions)
	if err != nil {
		return cliOptions{}, err
	}
	cases := make([]baseline.Case, 0, len(flowCounts)*len(directionValues))
	for _, direction := range directionValues {
		for _, parallel := range flowCounts {
			cases = append(cases, baseline.Case{Direction: direction, Parallel: parallel})
		}
	}

	resolvedRevision := strings.TrimSpace(*revision)
	if resolvedRevision == "" {
		resolvedRevision = strings.TrimSpace(revisionDetector())
	}
	if resolvedRevision == "" {
		return cliOptions{}, fmt.Errorf("source revision is unavailable; pass -revision explicitly")
	}
	if strings.TrimSpace(*outputRoot) == "" {
		return cliOptions{}, fmt.Errorf("-output must not be empty")
	}
	timestamp := now().UTC().Format("20060102T150405.000000000Z")
	finalOutput := filepath.Join(strings.TrimSpace(*outputRoot), strings.TrimSpace(*scenarioName)+"-"+timestamp)
	config := baseline.RunConfig{
		Scenario: baseline.Scenario{
			Name:          strings.TrimSpace(*scenarioName),
			TURNTransport: strings.ToLower(strings.TrimSpace(*turnTransport)),
			ProxyMode:     strings.ToLower(strings.TrimSpace(*proxyMode)),
			Sessions:      *sessions,
		},
		Revision:        resolvedRevision,
		Target:          strings.TrimSpace(*target),
		IPerfExecutable: strings.TrimSpace(*iperfExecutable),
		DiagnosticsURL:  strings.TrimSpace(*diagnosticsURL),
		OutputDirectory: finalOutput,
		Runs:            *runs,
		WarmupRuns:      *warmups,
		DurationSeconds: int(*duration / time.Second),
		Settle:          *settle,
		Cooldown:        *cooldown,
		Cases:           cases,
		Notes:           strings.TrimSpace(*notes),
	}
	if err := config.Validate(); err != nil {
		return cliOptions{}, err
	}
	if _, err := baseline.NewHTTPMetricsClient(config.DiagnosticsURL, strings.Repeat("0", 64)); err != nil {
		return cliOptions{}, err
	}
	return cliOptions{RunConfig: config, TokenFile: strings.TrimSpace(*tokenFile), DryRun: *dryRun}, nil
}

func parseFlows(raw string) ([]int, error) {
	parts := strings.Split(raw, ",")
	flows := make([]int, 0, len(parts))
	seen := make(map[int]struct{}, len(parts))
	for _, part := range parts {
		value, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("invalid -flows value %q", part)
		}
		benchmarkCase := baseline.Case{Direction: baseline.DirectionUpload, Parallel: value}
		if err := benchmarkCase.Validate(); err != nil {
			return nil, err
		}
		if _, ok := seen[value]; ok {
			return nil, fmt.Errorf("duplicate -flows value %d", value)
		}
		seen[value] = struct{}{}
		flows = append(flows, value)
	}
	if len(flows) == 0 {
		return nil, fmt.Errorf("-flows must not be empty")
	}
	return flows, nil
}

func parseDirections(raw string) ([]baseline.Direction, error) {
	parts := strings.Split(raw, ",")
	directions := make([]baseline.Direction, 0, len(parts))
	seen := make(map[baseline.Direction]struct{}, len(parts))
	for _, part := range parts {
		direction := baseline.Direction(strings.ToLower(strings.TrimSpace(part)))
		benchmarkCase := baseline.Case{Direction: direction, Parallel: 1}
		if err := benchmarkCase.Validate(); err != nil {
			return nil, err
		}
		if _, ok := seen[direction]; ok {
			return nil, fmt.Errorf("duplicate -directions value %s", direction)
		}
		seen[direction] = struct{}{}
		directions = append(directions, direction)
	}
	if len(directions) == 0 {
		return nil, fmt.Errorf("-directions must not be empty")
	}
	return directions, nil
}

func detectBuildRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var revision string
	modified := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision != "" && modified {
		revision += "+dirty"
	}
	return revision
}

func makeDryRunPlan(config baseline.RunConfig) dryRunPlan {
	operations := len(config.Cases) * (config.WarmupRuns + config.Runs)
	estimated := time.Duration(operations*config.DurationSeconds) * time.Second
	estimated += time.Duration(len(config.Cases)*config.Runs) * config.Settle
	if operations > 1 {
		estimated += time.Duration(operations-1) * config.Cooldown
	}
	return dryRunPlan{
		Scenario: config.Scenario, Revision: config.Revision, Target: config.Target,
		IPerfExecutable: config.IPerfExecutable, DiagnosticsURL: config.DiagnosticsURL,
		OutputDirectory: config.OutputDirectory, Runs: config.Runs, WarmupRuns: config.WarmupRuns,
		Duration: (time.Duration(config.DurationSeconds) * time.Second).String(),
		Settle:   config.Settle.String(), Cooldown: config.Cooldown.String(), Cases: config.Cases,
		EstimatedDuration: estimated.String(), Notes: config.Notes,
	}
}
