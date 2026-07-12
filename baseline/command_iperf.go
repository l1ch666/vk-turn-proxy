package baseline

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type CommandIPerf struct {
	Path string
}

func (c CommandIPerf) Version(ctx context.Context) (string, error) {
	versionCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(versionCtx, c.Path, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w: %s", c.Path, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func (c CommandIPerf) Run(ctx context.Context, target string, durationSeconds int, benchmarkCase Case) (CommandResult, error) {
	arguments, err := IPerfArguments(target, durationSeconds, benchmarkCase)
	if err != nil {
		return CommandResult{}, err
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(durationSeconds)*time.Second+30*time.Second)
	defer cancel()
	command := exec.CommandContext(runCtx, c.Path, arguments...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err = command.Run()
	result := CommandResult{Arguments: arguments, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if runCtx.Err() != nil {
		return result, fmt.Errorf("iperf3 timed out or was cancelled: %w", runCtx.Err())
	}
	if err != nil {
		return result, fmt.Errorf("iperf3 exited unsuccessfully: %w", err)
	}
	return result, nil
}

func IPerfArguments(target string, durationSeconds int, benchmarkCase Case) ([]string, error) {
	if err := benchmarkCase.Validate(); err != nil {
		return nil, err
	}
	if durationSeconds < 1 || durationSeconds > 300 {
		return nil, fmt.Errorf("duration must be in 1..300 seconds")
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil || strings.TrimSpace(host) == "" || host != strings.TrimSpace(host) {
		return nil, fmt.Errorf("target must be a host:port address")
	}
	if portNumber, parseErr := strconv.Atoi(port); parseErr != nil || portNumber < 1 || portNumber > 65535 {
		return nil, fmt.Errorf("target port must be in 1..65535")
	}
	arguments := []string{
		"-c", host,
		"-p", port,
		"-J",
		"-t", strconv.Itoa(durationSeconds),
		"-P", strconv.Itoa(benchmarkCase.Parallel),
	}
	if benchmarkCase.Direction == DirectionDownload {
		arguments = append(arguments, "-R")
	}
	return arguments, nil
}
