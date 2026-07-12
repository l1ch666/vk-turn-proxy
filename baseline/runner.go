package baseline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/cacggghp/vk-turn-proxy/metrics"
)

type IPerfExecutor interface {
	Version(context.Context) (string, error)
	Run(context.Context, string, int, Case) (CommandResult, error)
}

type SnapshotFetcher interface {
	Fetch(context.Context) (metrics.Snapshot, error)
}

type CommandResult struct {
	Arguments []string
	Stdout    []byte
	Stderr    []byte
}

type RunConfig struct {
	Scenario        Scenario
	Revision        string
	Target          string
	IPerfExecutable string
	DiagnosticsURL  string
	OutputDirectory string
	Runs            int
	WarmupRuns      int
	DurationSeconds int
	Settle          time.Duration
	Cooldown        time.Duration
	Cases           []Case
	Notes           string
}

func (c RunConfig) Validate() error {
	if err := c.Scenario.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.Revision) == "" || len(c.Revision) > 128 {
		return fmt.Errorf("revision must be present and at most 128 characters")
	}
	host, rawPort, err := net.SplitHostPort(c.Target)
	if err != nil || strings.TrimSpace(host) == "" || host != strings.TrimSpace(host) {
		return fmt.Errorf("target must be a host:port address")
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("target port must be in 1..65535")
	}
	if strings.TrimSpace(c.IPerfExecutable) == "" {
		return fmt.Errorf("iperf3 executable must be set")
	}
	if strings.TrimSpace(c.DiagnosticsURL) == "" {
		return fmt.Errorf("diagnostics URL must be set")
	}
	if strings.TrimSpace(c.OutputDirectory) == "" {
		return fmt.Errorf("output directory must be set")
	}
	if c.Runs < MinimumRuns || c.Runs > 20 {
		return fmt.Errorf("measured runs must be in %d..20", MinimumRuns)
	}
	if c.WarmupRuns < 0 || c.WarmupRuns > 5 {
		return fmt.Errorf("warmup runs must be in 0..5")
	}
	if c.DurationSeconds < 1 || c.DurationSeconds > 300 {
		return fmt.Errorf("duration must be in 1..300 seconds")
	}
	if c.Settle < 0 || c.Settle > 30*time.Second {
		return fmt.Errorf("settle delay must be in 0..30s")
	}
	if c.Cooldown < 0 || c.Cooldown > 5*time.Minute {
		return fmt.Errorf("cooldown must be in 0..5m")
	}
	if len(c.Cases) == 0 {
		return fmt.Errorf("at least one benchmark case is required")
	}
	seen := make(map[Case]struct{}, len(c.Cases))
	for _, benchmarkCase := range c.Cases {
		if err := benchmarkCase.Validate(); err != nil {
			return err
		}
		if _, ok := seen[benchmarkCase]; ok {
			return fmt.Errorf("duplicate benchmark case %s", benchmarkCase.Slug())
		}
		seen[benchmarkCase] = struct{}{}
	}
	if len(c.Notes) > 4096 {
		return fmt.Errorf("notes must be at most 4096 characters")
	}
	return nil
}

type ArtifactFiles struct {
	IPerfJSON     string `json:"iperf_json"`
	IPerfStderr   string `json:"iperf_stderr"`
	MetricsBefore string `json:"metrics_before,omitempty"`
	MetricsAfter  string `json:"metrics_after,omitempty"`
}

type WarmupRecord struct {
	Case       Case          `json:"case"`
	Index      int           `json:"index"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	Arguments  []string      `json:"arguments"`
	IPerf      IPerfSummary  `json:"iperf"`
	Files      ArtifactFiles `json:"files"`
}

type RunRecord struct {
	Case         Case          `json:"case"`
	Index        int           `json:"index"`
	StartedAt    time.Time     `json:"started_at"`
	FinishedAt   time.Time     `json:"finished_at"`
	Arguments    []string      `json:"arguments"`
	IPerf        IPerfSummary  `json:"iperf"`
	MetricsDelta MetricDelta   `json:"metrics_delta"`
	Files        ArtifactFiles `json:"files"`
}

type Environment struct {
	Hostname  string `json:"hostname,omitempty"`
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
	GoVersion string `json:"go_version"`
}

type Report struct {
	SchemaVersion   int            `json:"schema_version"`
	Status          string         `json:"status"`
	Error           string         `json:"error,omitempty"`
	Scenario        Scenario       `json:"scenario"`
	Revision        string         `json:"revision"`
	Target          string         `json:"target"`
	IPerfExecutable string         `json:"iperf_executable"`
	IPerfVersion    string         `json:"iperf_version"`
	DiagnosticsURL  string         `json:"diagnostics_url"`
	Environment     Environment    `json:"environment"`
	RunsRequested   int            `json:"runs_requested"`
	WarmupRuns      int            `json:"warmup_runs"`
	DurationSeconds int            `json:"duration_seconds"`
	Settle          string         `json:"settle"`
	Cooldown        string         `json:"cooldown"`
	Cases           []Case         `json:"cases"`
	Notes           string         `json:"notes,omitempty"`
	StartedAt       time.Time      `json:"started_at"`
	FinishedAt      *time.Time     `json:"finished_at,omitempty"`
	InitialMetrics  string         `json:"initial_metrics_file"`
	Warmups         []WarmupRecord `json:"warmups"`
	Runs            []RunRecord    `json:"runs"`
	Summaries       []CaseSummary  `json:"summaries,omitempty"`
}

type Harness struct {
	IPerf   IPerfExecutor
	Metrics SnapshotFetcher
	Now     func() time.Time
	Sleep   func(context.Context, time.Duration) error
}

func (h Harness) Run(ctx context.Context, config RunConfig) (*Report, error) {
	if ctx == nil {
		return nil, fmt.Errorf("baseline context must not be nil")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if h.IPerf == nil || h.Metrics == nil {
		return nil, fmt.Errorf("baseline harness dependencies must not be nil")
	}
	if h.Now == nil {
		h.Now = time.Now
	}
	if h.Sleep == nil {
		h.Sleep = waitContext
	}
	if err := validateOutputAvailable(config.OutputDirectory); err != nil {
		return nil, err
	}

	iperfVersion, err := h.IPerf.Version(ctx)
	if err != nil {
		return nil, fmt.Errorf("query iperf3 version: %w", err)
	}
	initialMetrics, err := h.Metrics.Fetch(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch initial diagnostics: %w", err)
	}
	if err := validateReady(config.Scenario, initialMetrics); err != nil {
		return nil, err
	}
	if err := createOutputDirectory(config.OutputDirectory); err != nil {
		return nil, err
	}

	hostname, _ := os.Hostname()
	report := &Report{
		SchemaVersion:   1,
		Status:          "running",
		Scenario:        config.Scenario,
		Revision:        config.Revision,
		Target:          config.Target,
		IPerfExecutable: config.IPerfExecutable,
		IPerfVersion:    strings.TrimSpace(iperfVersion),
		DiagnosticsURL:  config.DiagnosticsURL,
		Environment: Environment{
			Hostname:  hostname,
			GOOS:      runtime.GOOS,
			GOARCH:    runtime.GOARCH,
			GoVersion: runtime.Version(),
		},
		RunsRequested:   config.Runs,
		WarmupRuns:      config.WarmupRuns,
		DurationSeconds: config.DurationSeconds,
		Settle:          config.Settle.String(),
		Cooldown:        config.Cooldown.String(),
		Cases:           append([]Case(nil), config.Cases...),
		Notes:           config.Notes,
		StartedAt:       h.Now().UTC(),
		InitialMetrics:  "initial.metrics.json",
		Warmups:         make([]WarmupRecord, 0, len(config.Cases)*config.WarmupRuns),
		Runs:            make([]RunRecord, 0, len(config.Cases)*config.Runs),
	}
	if err := writeJSON(filepath.Join(config.OutputDirectory, report.InitialMetrics), initialMetrics); err != nil {
		return nil, err
	}
	if err := persistReport(config.OutputDirectory, report); err != nil {
		return nil, err
	}

	totalOperations := len(config.Cases) * (config.WarmupRuns + config.Runs)
	completedOperations := 0
	fail := func(runErr error) (*Report, error) {
		report.Status = "failed"
		report.Error = runErr.Error()
		finishedAt := h.Now().UTC()
		report.FinishedAt = &finishedAt
		if persistErr := persistReport(config.OutputDirectory, report); persistErr != nil {
			runErr = errors.Join(runErr, persistErr)
		}
		return report, runErr
	}

	for _, benchmarkCase := range config.Cases {
		for index := 1; index <= config.WarmupRuns; index++ {
			prefix := fmt.Sprintf("warmup-%s-%02d", benchmarkCase.Slug(), index)
			startedAt := h.Now().UTC()
			result, runErr := h.IPerf.Run(ctx, config.Target, config.DurationSeconds, benchmarkCase)
			finishedAt := h.Now().UTC()
			files, artifactErr := writeIPerfArtifacts(config.OutputDirectory, prefix, result)
			if artifactErr != nil {
				return fail(artifactErr)
			}
			if runErr != nil {
				return fail(fmt.Errorf("warmup %s %d: %w", benchmarkCase.Slug(), index, runErr))
			}
			summary, parseErr := ParseIPerfJSON(result.Stdout)
			if parseErr != nil {
				return fail(fmt.Errorf("warmup %s %d: %w", benchmarkCase.Slug(), index, parseErr))
			}
			if err := validateIPerfCase(benchmarkCase, summary); err != nil {
				return fail(fmt.Errorf("warmup %s %d: %w", benchmarkCase.Slug(), index, err))
			}
			report.Warmups = append(report.Warmups, WarmupRecord{
				Case: benchmarkCase, Index: index, StartedAt: startedAt, FinishedAt: finishedAt,
				Arguments: append([]string(nil), result.Arguments...), IPerf: summary, Files: files,
			})
			completedOperations++
			if err := persistReport(config.OutputDirectory, report); err != nil {
				return fail(err)
			}
			if completedOperations < totalOperations && config.Cooldown > 0 {
				if err := h.Sleep(ctx, config.Cooldown); err != nil {
					return fail(err)
				}
			}
		}

		for index := 1; index <= config.Runs; index++ {
			prefix := fmt.Sprintf("run-%s-%02d", benchmarkCase.Slug(), index)
			before, fetchErr := h.Metrics.Fetch(ctx)
			if fetchErr != nil {
				return fail(fmt.Errorf("run %s %d metrics before: %w", benchmarkCase.Slug(), index, fetchErr))
			}
			startedAt := h.Now().UTC()
			result, runErr := h.IPerf.Run(ctx, config.Target, config.DurationSeconds, benchmarkCase)
			finishedAt := h.Now().UTC()
			var (
				after    metrics.Snapshot
				afterErr error
			)
			if config.Settle > 0 {
				afterErr = h.Sleep(ctx, config.Settle)
			}
			if afterErr == nil {
				after, afterErr = h.Metrics.Fetch(ctx)
			}

			var afterPointer *metrics.Snapshot
			if afterErr == nil {
				afterPointer = &after
			}
			files, artifactErr := writeRunArtifacts(config.OutputDirectory, prefix, result, before, afterPointer)
			if artifactErr != nil {
				return fail(artifactErr)
			}
			if runErr != nil {
				return fail(fmt.Errorf("run %s %d: %w", benchmarkCase.Slug(), index, runErr))
			}
			if afterErr != nil {
				return fail(fmt.Errorf("run %s %d metrics after: %w", benchmarkCase.Slug(), index, afterErr))
			}
			summary, parseErr := ParseIPerfJSON(result.Stdout)
			if parseErr != nil {
				return fail(fmt.Errorf("run %s %d: %w", benchmarkCase.Slug(), index, parseErr))
			}
			if err := validateIPerfCase(benchmarkCase, summary); err != nil {
				return fail(fmt.Errorf("run %s %d: %w", benchmarkCase.Slug(), index, err))
			}
			delta, deltaErr := DiffMetrics(before, after)
			if deltaErr != nil {
				return fail(fmt.Errorf("run %s %d: %w", benchmarkCase.Slug(), index, deltaErr))
			}
			report.Runs = append(report.Runs, RunRecord{
				Case: benchmarkCase, Index: index, StartedAt: startedAt, FinishedAt: finishedAt,
				Arguments: append([]string(nil), result.Arguments...), IPerf: summary, MetricsDelta: delta, Files: files,
			})
			completedOperations++
			if err := persistReport(config.OutputDirectory, report); err != nil {
				return fail(err)
			}
			if completedOperations < totalOperations && config.Cooldown > 0 {
				if err := h.Sleep(ctx, config.Cooldown); err != nil {
					return fail(err)
				}
			}
		}
	}

	measurements := make([]Measurement, len(report.Runs))
	for i, run := range report.Runs {
		measurements[i] = Measurement{Case: run.Case, IPerf: run.IPerf}
	}
	report.Summaries, err = Summarize(measurements)
	if err != nil {
		return fail(err)
	}
	report.Status = "complete"
	finishedAt := h.Now().UTC()
	report.FinishedAt = &finishedAt
	if err := persistReport(config.OutputDirectory, report); err != nil {
		return report, err
	}
	return report, nil
}

func validateReady(scenario Scenario, snapshot metrics.Snapshot) error {
	if snapshot.ActiveSessions < 1 {
		return fmt.Errorf("diagnostics reports no active transport session")
	}
	if scenario.ProxyMode == "bond" && snapshot.ActivePaths < 1 {
		return fmt.Errorf("diagnostics reports no active bonded path")
	}
	return nil
}

func validateIPerfCase(benchmarkCase Case, summary IPerfSummary) error {
	if !strings.EqualFold(summary.Protocol, "TCP") {
		return fmt.Errorf("iperf payload protocol is %q, want TCP", summary.Protocol)
	}
	if summary.RequestedStreams != benchmarkCase.Parallel {
		return fmt.Errorf("iperf stream count is %d, want %d", summary.RequestedStreams, benchmarkCase.Parallel)
	}
	wantReverse := benchmarkCase.Direction == DirectionDownload
	if summary.Reverse != wantReverse {
		return fmt.Errorf("iperf reverse=%t, want %t", summary.Reverse, wantReverse)
	}
	return nil
}

func createOutputDirectory(path string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return fmt.Errorf("create benchmark output parent: %w", err)
	}
	if err := os.Mkdir(path, 0o750); err != nil {
		return fmt.Errorf("create benchmark output directory: %w", err)
	}
	return nil
}

func validateOutputAvailable(path string) error {
	_, err := os.Stat(path)
	if err == nil {
		return fmt.Errorf("benchmark output directory already exists")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check benchmark output directory: %w", err)
	}
	return nil
}

func writeIPerfArtifacts(directory, prefix string, result CommandResult) (ArtifactFiles, error) {
	files := ArtifactFiles{
		IPerfJSON:   prefix + ".iperf.json",
		IPerfStderr: prefix + ".iperf.stderr.txt",
	}
	if err := os.WriteFile(filepath.Join(directory, files.IPerfJSON), result.Stdout, 0o640); err != nil {
		return ArtifactFiles{}, fmt.Errorf("write iperf JSON artifact: %w", err)
	}
	if err := os.WriteFile(filepath.Join(directory, files.IPerfStderr), result.Stderr, 0o640); err != nil {
		return ArtifactFiles{}, fmt.Errorf("write iperf stderr artifact: %w", err)
	}
	return files, nil
}

func writeRunArtifacts(directory, prefix string, result CommandResult, before metrics.Snapshot, after *metrics.Snapshot) (ArtifactFiles, error) {
	files, err := writeIPerfArtifacts(directory, prefix, result)
	if err != nil {
		return ArtifactFiles{}, err
	}
	files.MetricsBefore = prefix + ".metrics-before.json"
	if err := writeJSON(filepath.Join(directory, files.MetricsBefore), before); err != nil {
		return ArtifactFiles{}, err
	}
	if after != nil {
		files.MetricsAfter = prefix + ".metrics-after.json"
		if err := writeJSON(filepath.Join(directory, files.MetricsAfter), *after); err != nil {
			return ArtifactFiles{}, err
		}
	}
	return files, nil
}

func persistReport(directory string, report *Report) error {
	return writeJSON(filepath.Join(directory, "report.json"), report)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o640); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
