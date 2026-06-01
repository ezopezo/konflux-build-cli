package cliwrappers

import (
	"errors"
	"strings"

	l "github.com/konflux-ci/konflux-build-cli/pkg/logger"
)

var syftLog = l.Logger.WithField("logger", "SyftCli")

type SyftCliInterface interface {
	Scan(args *SyftScanArgs) (string, error)
}

var _ SyftCliInterface = &SyftCli{}

type SyftCli struct {
	Executor CliExecutorInterface
}

func NewSyftCli(executor CliExecutorInterface) (*SyftCli, error) {
	syftCliAvailable, err := CheckCliToolAvailable("syft")
	if err != nil {
		return nil, err
	}
	if !syftCliAvailable {
		return nil, errors.New("syft CLI is not available")
	}

	return &SyftCli{
		Executor: executor,
	}, nil
}

type SyftScanArgs struct {
	// The source to scan, e.g. an image reference or directory path. Required.
	// Can include the scheme prefix, e.g. dir:<path>, oci-archive:<image-tar>.
	Source string
	// The output format, e.g. cyclonedx-json, spdx-json, spdx-json@2.3. Required.
	Format string
	// Write the output to a file instead of stdout.
	// If specified, the string return value from Scan() will be empty.
	OutputFile string
	// Override the base set of catalogers to use.
	OverrideDefaultCatalogers string
	// Increase verbosity (1=info, 2=debug). By default, Syft doesn't log anything.
	Verbosity int
	// Run the scan from within the specified workdir to make Syft search for config files there.
	Workdir string
}

func (s *SyftCli) Scan(args *SyftScanArgs) (string, error) {
	if args.Source == "" {
		return "", errors.New("source to scan is empty")
	}
	if args.Format == "" {
		return "", errors.New("format is empty")
	}

	output := args.Format
	if args.OutputFile != "" {
		output += "=" + args.OutputFile
	}

	cmd := Command("syft", "scan", args.Source, "--output="+output)
	cmd.Dir = args.Workdir

	if args.Verbosity > 0 {
		cmd.Args = append(cmd.Args, "-"+strings.Repeat("v", args.Verbosity))
		cmd.LogOutput = true
	}

	if args.OverrideDefaultCatalogers != "" {
		cmd.Args = append(cmd.Args, "--override-default-catalogers="+args.OverrideDefaultCatalogers)
	}

	syftLog.Debugf("Running command:\n%s", shellJoin(cmd.Name, cmd.Args...))

	stdout, stderr, _, err := s.Executor.Execute(cmd)
	if err != nil {
		syftLog.Errorf("syft scan failed: %s", err.Error())
		if stderr != "" {
			syftLog.Errorf("stderr:\n%s", stderr)
		}
		return "", err
	}

	return stdout, nil
}
