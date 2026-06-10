package cliwrappers

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/konflux-ci/konflux-build-cli/pkg/common"

	l "github.com/konflux-ci/konflux-build-cli/pkg/logger"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

var buildahLog = l.Logger.WithField("logger", "BuildahCli")

type BuildahCliInterface interface {
	Build(args *BuildahBuildArgs) error
	Push(args *BuildahPushArgs) (string, error)
	Pull(args *BuildahPullArgs) error
	Inspect(args *BuildahInspectArgs) (string, error)
	InspectImage(name string) (BuildahImageInfo, error)
	Images(args *BuildahImagesArgs) (string, error)
	ImagesJson(args *BuildahImagesArgs) ([]BuildahImagesEntry, error)
	Version() (BuildahVersionInfo, error)
	ManifestCreate(args *BuildahManifestCreateArgs) error
	ManifestAdd(args *BuildahManifestAddArgs) error
	ManifestInspect(args *BuildahManifestInspectArgs) (string, error)
	ManifestPush(args *BuildahManifestPushArgs) (string, error)
}

var _ BuildahCliInterface = &BuildahCli{}

type BuildahCli struct {
	Executor CliExecutorInterface
}

func NewBuildahCli(executor CliExecutorInterface) (*BuildahCli, error) {
	buildahCliAvailable, err := CheckCliToolAvailable("buildah")
	if err != nil {
		return nil, err
	}
	if !buildahCliAvailable {
		return nil, errors.New("buildah CLI is not available")
	}

	return &BuildahCli{
		Executor: executor,
	}, nil
}

type BuildahBuildArgs struct {
	Containerfile    string
	ContextDir       string
	OutputRef        string
	Secrets          []BuildahSecret
	Volumes          []BuildahVolume
	BuildContexts    []BuildahBuildContext
	BuildArgs        []string
	BuildArgsFile    string
	Envs             []string
	Labels           []string
	Annotations      []string
	SourceDateEpoch  string
	RewriteTimestamp bool
	// Defaults to true in the CLI, need a way to distinguish between explicitly false and unset
	InheritLabels    *bool
	Target           string
	SkipUnusedStages *bool
	TLSVerify        *bool
	Squash           bool
	OmitHistory      bool
	NoCache          bool
	SecurityOpts     []string
	CapAdd           []string
	CapDrop          []string
	Devices          []string
	Ulimits          []string
	ExtraArgs        []string
	Wrapper          *WrapperCmd
}

type BuildahSecret struct {
	Src string
	Id  string
}

// Represents a buildah --volume argument: HOST-DIR:CONTAINER-DIR[:OPTIONS]
type BuildahVolume struct {
	HostDir      string
	ContainerDir string
	Options      string
}

// Represents a buildah --build-context argument: name=path
// (buildah also supports other sources for contexts, but we only support paths for now)
type BuildahBuildContext struct {
	Name     string
	Location string
}

// Check that the build arguments are valid, e.g. required arguments are set.
// Also called automatically by the BuildahCli.Build() method.
func (args *BuildahBuildArgs) Validate() error {
	if args.Containerfile == "" {
		return errors.New("containerfile path is empty")
	}
	if args.ContextDir == "" {
		return errors.New("context directory is empty")
	}
	if args.OutputRef == "" {
		return errors.New("output-ref is empty")
	}
	for _, volume := range args.Volumes {
		if strings.ContainsRune(volume.HostDir, ':') {
			return fmt.Errorf("':' in volume mount source path: %s", volume.HostDir)
		}
		if strings.ContainsRune(volume.ContainerDir, ':') {
			return fmt.Errorf("':' in volume mount target path: %s", volume.ContainerDir)
		}
	}
	return nil
}

// Make all paths (containerfile, context dir, secret files, ...) absolute.
func (args *BuildahBuildArgs) MakePathsAbsolute(baseDir string) error {
	ensureAbsolute := func(path *string) error {
		if filepath.IsAbs(*path) {
			return nil
		}
		abspath, err := filepath.Abs(filepath.Join(baseDir, *path))
		if err != nil {
			return fmt.Errorf("finding absolute path of %s in %s: %w", *path, baseDir, err)
		}
		*path = abspath
		return nil
	}

	err := ensureAbsolute(&args.Containerfile)
	if err != nil {
		return err
	}

	err = ensureAbsolute(&args.ContextDir)
	if err != nil {
		return err
	}

	for i := range args.Secrets {
		err = ensureAbsolute(&args.Secrets[i].Src)
		if err != nil {
			return err
		}
	}

	for i := range args.Volumes {
		err := ensureAbsolute(&args.Volumes[i].HostDir)
		if err != nil {
			return err
		}
	}

	for i := range args.BuildContexts {
		err := ensureAbsolute(&args.BuildContexts[i].Location)
		if err != nil {
			return err
		}
	}

	if args.BuildArgsFile != "" {
		err = ensureAbsolute(&args.BuildArgsFile)
		if err != nil {
			return err
		}
	}

	return nil
}

func (b *BuildahCli) Build(args *BuildahBuildArgs) error {
	if err := args.Validate(); err != nil {
		return fmt.Errorf("validating buildah args: %w", err)
	}

	buildahArgs := []string{"build", "--file", args.Containerfile, "--tag", args.OutputRef}

	for _, secret := range args.Secrets {
		secretArg := "src=" + secret.Src + ",id=" + secret.Id
		buildahArgs = append(buildahArgs, "--secret="+secretArg)
	}

	for _, volume := range args.Volumes {
		volumeArg := volume.HostDir + ":" + volume.ContainerDir
		if volume.Options != "" {
			volumeArg += ":" + volume.Options
		}
		buildahArgs = append(buildahArgs, "--volume="+volumeArg)
	}

	for _, buildcontext := range args.BuildContexts {
		buildahArgs = append(buildahArgs, "--build-context="+buildcontext.Name+"="+buildcontext.Location)
	}

	for _, buildArg := range args.BuildArgs {
		buildahArgs = append(buildahArgs, "--build-arg="+buildArg)
	}

	if args.BuildArgsFile != "" {
		buildahArgs = append(buildahArgs, "--build-arg-file="+args.BuildArgsFile)
	}

	for _, env := range args.Envs {
		buildahArgs = append(buildahArgs, "--env="+env)
	}

	for _, label := range args.Labels {
		buildahArgs = append(buildahArgs, "--label="+label)
	}

	for _, annotation := range args.Annotations {
		buildahArgs = append(buildahArgs, "--annotation="+annotation)
	}

	if args.SourceDateEpoch != "" {
		buildahArgs = append(buildahArgs, "--source-date-epoch="+args.SourceDateEpoch)
	}

	if args.RewriteTimestamp {
		buildahArgs = append(buildahArgs, "--rewrite-timestamp")
	}

	if args.InheritLabels != nil {
		buildahArgs = append(buildahArgs, fmt.Sprintf("--inherit-labels=%t", *args.InheritLabels))
	}

	if args.Target != "" {
		buildahArgs = append(buildahArgs, "--target="+args.Target)
	}

	if args.SkipUnusedStages != nil {
		buildahArgs = append(buildahArgs, fmt.Sprintf("--skip-unused-stages=%t", *args.SkipUnusedStages))
	}

	if args.TLSVerify != nil {
		buildahArgs = append(buildahArgs, fmt.Sprintf("--tls-verify=%t", *args.TLSVerify))
	}

	if args.Squash {
		buildahArgs = append(buildahArgs, "--squash")
	}

	if args.OmitHistory {
		buildahArgs = append(buildahArgs, "--omit-history")
	}

	if args.NoCache {
		buildahArgs = append(buildahArgs, "--no-cache")
	}

	for _, opt := range args.SecurityOpts {
		buildahArgs = append(buildahArgs, "--security-opt="+opt)
	}

	for _, capability := range args.CapAdd {
		buildahArgs = append(buildahArgs, "--cap-add="+capability)
	}

	for _, capability := range args.CapDrop {
		buildahArgs = append(buildahArgs, "--cap-drop="+capability)
	}

	for _, dev := range args.Devices {
		buildahArgs = append(buildahArgs, "--device="+dev)
	}

	for _, ulimit := range args.Ulimits {
		buildahArgs = append(buildahArgs, "--ulimit="+ulimit)
	}

	// Append extra arguments before the context directory
	buildahArgs = append(buildahArgs, args.ExtraArgs...)
	// Context directory must be the last argument
	buildahArgs = append(buildahArgs, args.ContextDir)

	executable := "buildah"
	if args.Wrapper != nil {
		executable, buildahArgs = args.Wrapper.Wrap(executable, buildahArgs)
	}

	buildahLog.Debugf("Running command:\n%s", shellJoin(executable, buildahArgs...))

	_, _, _, err := b.Executor.Execute(Cmd{
		Name: executable, Args: buildahArgs,
		// Prefix logs with "buildah" regardless of the wrappers used
		NameInLogs: "buildah", LogOutput: true,
	})
	if err != nil {
		buildahLog.Errorf("buildah build failed: %s", err.Error())
		return err
	}

	buildahLog.Debug("Build completed successfully")

	return nil
}

type BuildahPushArgs struct {
	Image       string
	Destination string
	TLSVerify   *bool
}

// Push an image from local storage to the registry. Return the digest of the pushed manifest.
func (b *BuildahCli) Push(args *BuildahPushArgs) (string, error) {
	if args.Image == "" {
		return "", errors.New("image arg is empty")
	}

	// Create temp file for digest
	tmpFile, err := os.CreateTemp("", "buildah-digest-")
	if err != nil {
		return "", err
	}
	digestFile := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(digestFile) }()

	buildahArgs := []string{"push", "--digestfile", digestFile}
	if args.TLSVerify != nil {
		buildahArgs = append(buildahArgs, fmt.Sprintf("--tls-verify=%t", *args.TLSVerify))
	}
	buildahArgs = append(buildahArgs, args.Image)
	if args.Destination != "" {
		buildahArgs = append(buildahArgs, args.Destination)
	}

	buildahLog.Debugf("Running command:\n%s", shellJoin("buildah", buildahArgs...))

	retryer := NewRetryer(func() (string, string, int, error) {
		return b.Executor.Execute(Cmd{Name: "buildah", Args: buildahArgs, LogOutput: true})
	}).WithImageRegistryPreset().
		StopIfOutputContains("unauthorized").
		StopIfOutputContains("authentication required")

	_, _, _, err = retryer.Run()
	if err != nil {
		buildahLog.Errorf("buildah push failed: %s", err.Error())
		return "", err
	}

	buildahLog.Debug("Push completed successfully")

	content, err := os.ReadFile(digestFile)
	if err != nil {
		return "", err
	}

	digest := strings.TrimSpace(string(content))
	return digest, nil
}

type BuildahPullArgs struct {
	Image     string
	HttpProxy string // Sets HTTP_PROXY and HTTPS_PROXY for the pull command
	NoProxy   string // Sets NO_PROXY for the pull command
	TLSVerify *bool
	ExtraEnv  []string // Sets extra env vars. Lower precedence than the proxy variables.
}

// Pull an image from the registry to local storage.
func (b *BuildahCli) Pull(args *BuildahPullArgs) error {
	if args.Image == "" {
		return errors.New("image arg is empty")
	}

	buildahArgs := []string{"pull"}
	if args.TLSVerify != nil {
		buildahArgs = append(buildahArgs, fmt.Sprintf("--tls-verify=%t", *args.TLSVerify))
	}
	buildahArgs = append(buildahArgs, args.Image)

	buildahLog.Debugf("Running command:\n%s", shellJoin("buildah", buildahArgs...))

	cmd := Cmd{Name: "buildah", Args: buildahArgs, LogOutput: true}
	env := slices.Concat(args.ExtraEnv, common.ProxyEnvVars(args.HttpProxy, args.NoProxy))
	if len(env) > 0 {
		// Note: this overrides proxy vars already set in the environment, if any (last value wins)
		cmd.Env = append(os.Environ(), env...)
	}

	retryer := NewRetryer(func() (string, string, int, error) {
		return b.Executor.Execute(cmd)
	}).WithImageRegistryPreset().
		StopIfOutputContains("unauthorized").
		StopIfOutputContains("authentication required").
		StopIfOutputContains("no image found in image index for architecture")

	_, _, _, err := retryer.Run()
	if err != nil {
		buildahLog.Errorf("buildah pull failed: %s", err.Error())
		return err
	}

	buildahLog.Debug("Pull completed successfully")

	return nil
}

type BuildahInspectArgs struct {
	// Name of object to inspect, required
	Name string
	// container | image | manifest, required
	// Note: Buildah does not require this one, but the behavior is not fully specified
	// if two objects of different Type have the same Name. Make it required to be safe.
	Type string
}

func (b *BuildahCli) Inspect(args *BuildahInspectArgs) (string, error) {
	if args.Name == "" {
		return "", errors.New("name is empty")
	}
	if args.Type == "" {
		return "", errors.New("type is empty")
	}

	buildahArgs := []string{"inspect", "--type", args.Type, args.Name}

	buildahLog.Debugf("Running command:\n%s", shellJoin("buildah", buildahArgs...))

	stdout, stderr, _, err := b.Executor.Execute(Command("buildah", buildahArgs...))
	if err != nil {
		buildahLog.Errorf("buildah inspect failed: %s", err.Error())
		if stderr != "" {
			buildahLog.Errorf("stderr:\n%s", stderr)
		}
		return "", err
	}

	return stdout, nil
}

// The default output of Inspect() for the 'image' Type (a single image, not an image index).
// Includes a subset of the attributes that buildah returns.
type BuildahImageInfo struct {
	OCIv1 ociv1.Image
}

func (b *BuildahCli) InspectImage(name string) (BuildahImageInfo, error) {
	jsonOutput, err := b.Inspect(&BuildahInspectArgs{
		Name: name,
		Type: "image",
	})
	if err != nil {
		return BuildahImageInfo{}, err
	}

	var imageInfo BuildahImageInfo

	err = json.Unmarshal([]byte(jsonOutput), &imageInfo)
	if err != nil {
		return BuildahImageInfo{}, fmt.Errorf("parsing inspect output: %w", err)
	}

	return imageInfo, nil
}

type BuildahImagesArgs struct {
	// Optional image name to filter the output to a specific image.
	Image string
	// Whether to pass --json to buildah images.
	Json bool
}

// A subset of the JSON output of `buildah images --json`.
type BuildahImagesEntry struct {
	// All the names (including tag) that have been used to pull this image,
	// resolved to the fully qualified name (includes the registry domain).
	Names []string `json:"names"`
	// The digest of either the manifest list or the manifest for this image.
	// If the first pull of this image resolved directly to a manifest, this will be
	// the manifest digest (e.g. the reference was pinned directly to a manifest digest,
	// or the tag resolves to a manifest, not a manifest list).
	// Otherwise, this will be the manifest list digest.
	Digest string `json:"digest"`
}

// List images in local storage, optionally filtering by name.
func (b *BuildahCli) Images(args *BuildahImagesArgs) (string, error) {
	buildahArgs := []string{"images"}

	if args.Json {
		buildahArgs = append(buildahArgs, "--json")
	}
	if args.Image != "" {
		buildahArgs = append(buildahArgs, args.Image)
	}

	buildahLog.Debugf("Running command:\n%s", shellJoin("buildah", buildahArgs...))

	stdout, stderr, _, err := b.Executor.Execute(Command("buildah", buildahArgs...))
	if err != nil {
		buildahLog.Errorf("buildah images failed: %s", err.Error())
		if stderr != "" {
			buildahLog.Errorf("stderr:\n%s", stderr)
		}
		return "", err
	}

	return stdout, nil
}

// List images in local storage and parse the JSON output.
func (b *BuildahCli) ImagesJson(args *BuildahImagesArgs) ([]BuildahImagesEntry, error) {
	jsonArgs := *args
	jsonArgs.Json = true
	stdout, err := b.Images(&jsonArgs)
	if err != nil {
		return nil, err
	}

	var images []BuildahImagesEntry
	err = json.Unmarshal([]byte(stdout), &images)
	if err != nil {
		return nil, fmt.Errorf("parsing images output: %w", err)
	}

	return images, nil
}

type BuildahVersionInfo struct {
	Version string `json:"version"`
}

// Parse the "{major}.{minor}.{patch}" Version string into a 3-element int slice.
func (v BuildahVersionInfo) ParseVersion() ([]int, error) {
	if v.Version == "" {
		return nil, errors.New("buildah version is empty")
	}

	versionParts := strings.Split(v.Version, ".")

	version := make([]int, 0, 3)
	for _, part := range versionParts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, fmt.Errorf("negative number in version: %d", n)
		}
		version = append(version, n)
	}

	if len(version) != 3 {
		return nil, fmt.Errorf("expected 3-part version number, got %q", v.Version)
	}

	return version, nil
}

func (b *BuildahCli) Version() (BuildahVersionInfo, error) {
	buildahArgs := []string{"version", "--json"}

	buildahLog.Debugf("Running command:\n%s", shellJoin("buildah", buildahArgs...))

	stdout, stderr, _, err := b.Executor.Execute(Command("buildah", buildahArgs...))
	if err != nil {
		buildahLog.Errorf("buildah version failed: %s", err.Error())
		if stderr != "" {
			buildahLog.Errorf("stderr:\n%s", stderr)
		}
		return BuildahVersionInfo{}, err
	}

	var versionInfo BuildahVersionInfo
	err = json.Unmarshal([]byte(stdout), &versionInfo)
	if err != nil {
		return BuildahVersionInfo{}, fmt.Errorf("parsing version output: %w", err)
	}

	return versionInfo, nil
}

type BuildahManifestCreateArgs struct {
	ManifestName string
}

// ManifestCreate creates a new manifest list
func (b *BuildahCli) ManifestCreate(args *BuildahManifestCreateArgs) error {
	if args.ManifestName == "" {
		return errors.New("manifest name is empty")
	}

	buildahArgs := []string{"manifest", "create", args.ManifestName}

	buildahLog.Debugf("Running command:\nbuildah %s", strings.Join(buildahArgs, " "))

	_, _, _, err := b.Executor.Execute(Cmd{Name: "buildah", Args: buildahArgs, LogOutput: true})
	if err != nil {
		buildahLog.Errorf("buildah manifest create failed: %s", err.Error())
		return err
	}

	buildahLog.Debug("Manifest create completed successfully")

	return nil
}

type BuildahManifestAddArgs struct {
	ManifestName string
	ImageRef     string
	All          bool
}

// ManifestAdd adds an image to a manifest list
func (b *BuildahCli) ManifestAdd(args *BuildahManifestAddArgs) error {
	if args.ManifestName == "" {
		return errors.New("manifest name is empty")
	}
	if args.ImageRef == "" {
		return errors.New("image reference is empty")
	}

	buildahArgs := []string{"manifest", "add", args.ManifestName, args.ImageRef}

	if args.All {
		buildahArgs = append(buildahArgs, "--all")
	}

	buildahLog.Debugf("Running command:\nbuildah %s", strings.Join(buildahArgs, " "))

	_, _, _, err := b.Executor.Execute(Cmd{Name: "buildah", Args: buildahArgs, LogOutput: true})
	if err != nil {
		buildahLog.Errorf("buildah manifest add failed: %s", err.Error())
		return err
	}

	buildahLog.Debug("Manifest add completed successfully")

	return nil
}

type BuildahManifestInspectArgs struct {
	ManifestName string
}

// ManifestInspect inspects a manifest list and returns the JSON output
func (b *BuildahCli) ManifestInspect(args *BuildahManifestInspectArgs) (string, error) {
	if args.ManifestName == "" {
		return "", errors.New("manifest name is empty")
	}

	buildahArgs := []string{"manifest", "inspect", args.ManifestName}

	buildahLog.Debugf("Running command:\nbuildah %s", strings.Join(buildahArgs, " "))

	stdout, _, _, err := b.Executor.Execute(Command("buildah", buildahArgs...))
	if err != nil {
		buildahLog.Errorf("buildah manifest inspect failed: %s", err.Error())
		return "", err
	}

	buildahLog.Debug("Manifest inspect completed successfully")

	return stdout, nil
}

type BuildahManifestPushArgs struct {
	ManifestName string
	Destination  string
	Format       string
	TLSVerify    bool
}

// ManifestPush pushes a manifest list to a registry and returns the digest
func (b *BuildahCli) ManifestPush(args *BuildahManifestPushArgs) (string, error) {
	if args.ManifestName == "" {
		return "", errors.New("manifest name is empty")
	}
	if args.Destination == "" {
		return "", errors.New("destination is empty")
	}

	tmpFile, err := os.CreateTemp("", "buildah-manifest-digest-")
	if err != nil {
		return "", err
	}
	digestFile := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(digestFile) }()

	buildahArgs := []string{"manifest", "push", "--digestfile", digestFile}

	if args.Format != "" {
		buildahArgs = append(buildahArgs, "--format", args.Format)
	}

	if args.TLSVerify {
		buildahArgs = append(buildahArgs, "--tls-verify=true")
	} else {
		buildahArgs = append(buildahArgs, "--tls-verify=false")
	}

	buildahArgs = append(buildahArgs, args.ManifestName, args.Destination)

	buildahLog.Debugf("Running command:\nbuildah %s", strings.Join(buildahArgs, " "))

	retryer := NewRetryer(func() (string, string, int, error) {
		return b.Executor.Execute(Cmd{Name: "buildah", Args: buildahArgs, LogOutput: true})
	}).WithImageRegistryPreset().
		StopIfOutputContains("unauthorized").
		StopIfOutputContains("authentication required")

	_, _, _, err = retryer.Run()
	if err != nil {
		buildahLog.Errorf("buildah manifest push failed: %s", err.Error())
		return "", err
	}

	buildahLog.Debug("Manifest push completed successfully")

	content, err := os.ReadFile(digestFile)
	if err != nil {
		return "", err
	}

	digest := strings.TrimSpace(string(content))
	return digest, nil
}
