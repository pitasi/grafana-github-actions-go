package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

type CommandRunner interface {
	Run(ctx context.Context, command string, args ...string) (string, error)
}

type ShellCommandRunner struct {
	Logger *slog.Logger
}

func NewShellCommandRunner(log *slog.Logger) *ShellCommandRunner {
	return &ShellCommandRunner{
		Logger: log,
	}
}

func (r *ShellCommandRunner) Run(ctx context.Context, command string, args ...string) (string, error) {
	var (
		stdout = bytes.NewBuffer(nil)
		stderr = bytes.NewBuffer(nil)
		cmdstr = strings.Join(append([]string{command}, args...), " ")
	)
	pwd, _ := os.Getwd()

	log := r.Logger.With("wd", pwd)
	r.Logger.Debug(fmt.Sprintf("running command '%s'", cmdstr))

	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()

	log.Debug(stdout.String(), "stream", "stdout", "exit_code", cmd.ProcessState.ExitCode())
	log.Debug(stderr.String(), "stream", "stderr", "exit_code", cmd.ProcessState.ExitCode())

	if err != nil {
		return "", fmt.Errorf("error running command '%s'\nerror: %w\nstdout: %s\nstderr: %s", cmdstr, err, stdout.String(), stderr.String())
	}

	return strings.TrimSpace(stdout.String()), nil
}
