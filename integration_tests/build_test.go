package integration_tests

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	. "github.com/konflux-ci/konflux-build-cli/integration_tests/framework"
	"github.com/konflux-ci/konflux-build-cli/testutil"
)

const (
	BuildImage = TaskRunnerImageRef
	// Tests that need a real base image should try to use the same one when possible
	// (to reduce the time spent pulling base images)
	baseImage = "registry.access.redhat.com/ubi10/ubi-micro:10.1@sha256:2946fa1b951addbcd548ef59193dc0af9b3e9fedb0287b4ddb6e697b06581622"
)

type BuildParams struct {
	Context                 string
	Containerfile           string
	OutputRef               string
	Push                    bool
	SecretDirs              []string
	WorkdirMount            string
	BuildArgs               []string
	BuildArgsFile           string
	Envs                    []string
	Labels                  []string
	Annotations             []string
	AnnotationsFile         string
	ImageSource             string
	ImageRevision           string
	LegacyBuildTimestamp    string
	SourceDateEpoch         string
	RewriteTimestamp        bool
	QuayImageExpiresAfter   string
	AddLegacyLabels         bool
	ContainerfileJsonOutput string
	SkipInjections          bool
	// Defaults to true in the CLI, need a way to distinguish between explicitly false and unset
	InheritLabels              *bool
	IncludeLegacyBuildinfoPath bool
	Target                     string
	SkipUnusedStages           *bool
	Hermetic                   bool
	ImagePullProxy             string
	ImagePullNoProxy           string
	YumReposDSources           []string
	YumReposDTarget            string
	PrefetchDir                string
	PrefetchDirCopy            string
	PrefetchOutputMount        string
	PrefetchEnvMount           string
	ResolvedBaseImagesOutput   string
	RHSMEntitlements           string
	RHSMActivationKey          string
	RHSMOrg                    string
	RHSMActivationMount        string
	RHSMActivationPreregister  bool
	RHSMMountCACerts           string
	Squash                     bool
	OmitHistory                bool
	NoCache                    bool
	CapAdd                     []string
	CapDrop                    []string
	AllowCrossPlatformImages   bool
	SyftSourceOutput           string
	SyftImageOutput            string
	SBOMFormat                 string
	ExtraArgs                  []string
}

func boolptr(v bool) *bool {
	return &v
}

// Public interface for parity with ApplyTags. Not used in these tests directly.
func RunBuild(buildParams BuildParams, imageRegistry ImageRegistry) error {
	storagePath, err := createContainerStorageDir()
	defer removeContainerStorageDir(storagePath)
	if err != nil {
		return err
	}

	container, err := setupBuildContainer(buildParams, imageRegistry,
		WithUser("root"),
		maybeMountContainerStorage(storagePath, "root"),
	)
	defer container.DeleteIfExists()
	if err != nil {
		return err
	}

	return runBuild(container, buildParams)
}

// Creates a temporary directory for container storage in the repository root,
// under .test-containers-storage. Returns the full path to the created directory.
// This directory can be mounted at /var/lib/containers in the test runner container.
//
// When running on MacOS, doesn't create the directory and returns ("", nil).
// On macOS, containers run in a Linux VM. Overlay storage driver doesn't work reliably with
// host volume mounts through the VM, so don't use a shared directory on macOS.
//
// Why put the directory in the repository root:
//   - The directory can't be in /tmp, because that is usually a tmpfs which doesn't
//     support all the operations that buildah needs to do with /var/lib/containers.
//   - The repository root is an obvious choice for a directory that likely isn't in /tmp,
//     is writable for the current user and doesn't pollute the user's home directory.
func createContainerStorageDir() (string, error) {
	if runtime.GOOS == "darwin" {
		return "", nil
	}

	repoRoot, err := filepath.Abs(FindRepoRoot())
	if err != nil {
		return "", err
	}

	storageBaseDir := path.Join(repoRoot, ".test-containers-storage")
	if err := EnsureDirectory(storageBaseDir); err != nil {
		return "", err
	}

	tmpDir, err := os.MkdirTemp(storageBaseDir, "storage-")
	if err != nil {
		return "", err
	}

	// Tests may run the BuildImage as a non-root user. Allow this user to write to the
	// container storage dir (by allowing everyone to write).
	if err := os.Chmod(tmpDir, 0777); err != nil {
		return tmpDir, err
	}

	return tmpDir, nil
}

// Return a ContainerOption that mounts storagePath at the correct path for the specified user.
// If storagePath is empty, returns a no-op ContainerOption.
func maybeMountContainerStorage(storagePath string, forUser string) ContainerOption {
	if storagePath == "" {
		return func(*TestRunnerContainer) {}
	}

	if forUser == "root" {
		return WithVolumeWithOptions(storagePath, "/var/lib/containers", "z")
	}
	return WithVolumeWithOptions(storagePath, "/home/taskuser/.local/share/containers", "z")
}

// Try to remove a directory created by createContainerStorageDir,
// and the parent .test-containers-storage directory if empty.
// The cleanup is best-effort and ignores errors.
func removeContainerStorageDir(containerStoragePath string) {
	if containerStoragePath == "" {
		return
	}

	warnIfErr := func(err error) {
		if err != nil {
			fmt.Printf("WARNING: cleanup error: %s\n", err)
		}
	}

	// Try to clean up from inside a container
	err := cleanupContainerStorageDir(containerStoragePath)
	warnIfErr(err)

	// Container storage path should be an empty directory at this point
	err = os.Remove(containerStoragePath)
	warnIfErr(err)

	// Try to remove the parent .test-containers-storage directory. Will fail if it's not
	// empty (e.g. a different test process is running in parallel). This is fine. The last
	// test process that finishes should clean it up successfully.
	_ = os.Remove(filepath.Dir(containerStoragePath))
}

// Clean up the container storage dir from inside a container.
//
// We can't always clean up directly from the host, because the files in the containerStoragePath
// may be owned by a different UID than the host UID. They're owned by the container user UID,
// which may or may not be the same as the host UID depending on userns mapping.
func cleanupContainerStorageDir(containerStoragePath string) error {
	container := NewBuildCliRunnerContainer("kbc-build-cleanup", BuildImage)
	defer container.DeleteIfExists()

	// Clean up as root, which works regardless of whether the tests had run as root or as taskuser
	// (root can delete files owned by taskuser but not vice-versa).
	container.SetUser("root")
	container.AddVolumeWithOptions(containerStoragePath, "/var/lib/containers", "z")

	err := container.Start()
	if err != nil {
		return err
	}

	return container.ExecuteCommand("bash", "-c", "rm -rf /var/lib/containers/*")
}

// Creates and starts a container for running builds.
// The caller is responsible for cleaning up the container.
// Returns a non-nil container even if an error occurs. The caller should always call
// DeleteIfExists() on the container for cleanup.
func setupBuildContainer(buildParams BuildParams, imageRegistry ImageRegistry, opts ...ContainerOption) (*TestRunnerContainer, error) {
	container := NewBuildCliRunnerContainer("kbc-build", BuildImage, opts...)
	// As of buildah v1.44.0, the user running 'buildah build' must own the context directory.
	// Add :U to make podman change the owner to the container user.
	container.AddVolumeWithOptions(buildParams.Context, "/workspace", "z,U")

	var err error
	if imageRegistry != nil {
		err = container.StartWithRegistryIntegration(imageRegistry)
	} else {
		err = container.Start()
	}

	return container, err
}

// Executes the build command in the provided container.
func runBuild(container *TestRunnerContainer, buildParams BuildParams) error {
	_, _, err := runBuildWithOutput(container, buildParams)
	return err
}

// Executes the build command and returns stdout, stderr, and error.
func runBuildWithOutput(container *TestRunnerContainer, buildParams BuildParams) (string, string, error) {
	// Construct the build arguments
	args := []string{"image", "build"}
	args = append(args, "-t", buildParams.OutputRef)
	args = append(args, "-c", "/workspace")
	if buildParams.Containerfile != "" {
		args = append(args, "-f", buildParams.Containerfile)
	}
	if buildParams.Push {
		args = append(args, "--push")
	}
	// Add secret directories if provided
	if len(buildParams.SecretDirs) > 0 {
		args = append(args, "--secret-dirs")
		args = append(args, buildParams.SecretDirs...)
	}
	if buildParams.WorkdirMount != "" {
		args = append(args, "--workdir-mount", buildParams.WorkdirMount)
	}
	if len(buildParams.BuildArgs) > 0 {
		args = append(args, "--build-args")
		args = append(args, buildParams.BuildArgs...)
	}
	if buildParams.BuildArgsFile != "" {
		args = append(args, "--build-args-file", buildParams.BuildArgsFile)
	}
	if len(buildParams.Envs) > 0 {
		args = append(args, "--envs")
		args = append(args, buildParams.Envs...)
	}
	if len(buildParams.Labels) > 0 {
		args = append(args, "--labels")
		args = append(args, buildParams.Labels...)
	}
	if len(buildParams.Annotations) > 0 {
		args = append(args, "--annotations")
		args = append(args, buildParams.Annotations...)
	}
	if buildParams.AnnotationsFile != "" {
		args = append(args, "--annotations-file", buildParams.AnnotationsFile)
	}
	if buildParams.ImageSource != "" {
		args = append(args, "--image-source", buildParams.ImageSource)
	}
	if buildParams.ImageRevision != "" {
		args = append(args, "--image-revision", buildParams.ImageRevision)
	}
	if buildParams.LegacyBuildTimestamp != "" {
		args = append(args, "--legacy-build-timestamp", buildParams.LegacyBuildTimestamp)
	}
	if buildParams.SourceDateEpoch != "" {
		args = append(args, "--source-date-epoch", buildParams.SourceDateEpoch)
	}
	if buildParams.RewriteTimestamp {
		args = append(args, "--rewrite-timestamp")
	}
	if buildParams.QuayImageExpiresAfter != "" {
		args = append(args, "--quay-image-expires-after", buildParams.QuayImageExpiresAfter)
	}
	if buildParams.AddLegacyLabels {
		args = append(args, "--add-legacy-labels")
	}
	if buildParams.ContainerfileJsonOutput != "" {
		args = append(args, "--containerfile-json-output", buildParams.ContainerfileJsonOutput)
	}
	if buildParams.SkipInjections {
		args = append(args, "--skip-injections")
	}
	if buildParams.IncludeLegacyBuildinfoPath {
		args = append(args, "--include-legacy-buildinfo-path")
	}
	if buildParams.InheritLabels != nil {
		args = append(args, fmt.Sprintf("--inherit-labels=%t", *buildParams.InheritLabels))
	}
	if buildParams.Target != "" {
		args = append(args, "--target", buildParams.Target)
	}
	if buildParams.SkipUnusedStages != nil {
		args = append(args, fmt.Sprintf("--skip-unused-stages=%t", *buildParams.SkipUnusedStages))
	}
	if buildParams.Hermetic {
		args = append(args, "--hermetic")
	}
	if buildParams.ImagePullProxy != "" {
		args = append(args, "--image-pull-proxy", buildParams.ImagePullProxy)
	}
	if buildParams.ImagePullNoProxy != "" {
		args = append(args, "--image-pull-noproxy", buildParams.ImagePullNoProxy)
	}
	if len(buildParams.YumReposDSources) > 0 {
		args = append(args, "--yum-repos-d-sources")
		args = append(args, buildParams.YumReposDSources...)
	}
	if buildParams.YumReposDTarget != "" {
		args = append(args, "--yum-repos-d-target", buildParams.YumReposDTarget)
	}
	if buildParams.PrefetchDir != "" {
		args = append(args, "--prefetch-dir", buildParams.PrefetchDir)
	}
	if buildParams.PrefetchDirCopy != "" {
		args = append(args, "--prefetch-dir-copy", buildParams.PrefetchDirCopy)
	}
	if buildParams.PrefetchOutputMount != "" {
		args = append(args, "--prefetch-output-mount", buildParams.PrefetchOutputMount)
	}
	if buildParams.PrefetchEnvMount != "" {
		args = append(args, "--prefetch-env-mount", buildParams.PrefetchEnvMount)
	}
	if buildParams.ResolvedBaseImagesOutput != "" {
		args = append(args, "--resolved-base-images-output", buildParams.ResolvedBaseImagesOutput)
	}
	if buildParams.RHSMEntitlements != "" {
		args = append(args, "--rhsm-entitlements", buildParams.RHSMEntitlements)
	}
	if buildParams.RHSMActivationKey != "" {
		args = append(args, "--rhsm-activation-key", buildParams.RHSMActivationKey)
	}
	if buildParams.RHSMOrg != "" {
		args = append(args, "--rhsm-org", buildParams.RHSMOrg)
	}
	if buildParams.RHSMActivationMount != "" {
		args = append(args, "--rhsm-activation-mount", buildParams.RHSMActivationMount)
	}
	if buildParams.RHSMActivationPreregister {
		args = append(args, "--rhsm-activation-preregister")
	}
	if buildParams.RHSMMountCACerts != "" {
		args = append(args, "--rhsm-mount-ca-certs", buildParams.RHSMMountCACerts)
	}
	if buildParams.Squash {
		args = append(args, "--squash")
	}
	if buildParams.OmitHistory {
		args = append(args, "--omit-history")
	}
	if buildParams.NoCache {
		args = append(args, "--no-cache")
	}
	if len(buildParams.CapAdd) > 0 {
		args = append(args, "--cap-add")
		args = append(args, buildParams.CapAdd...)
	}
	if len(buildParams.CapDrop) > 0 {
		args = append(args, "--cap-drop")
		args = append(args, buildParams.CapDrop...)
	}
	if buildParams.AllowCrossPlatformImages {
		args = append(args, "--allow-cross-platform-images")
	}
	if buildParams.SyftSourceOutput != "" {
		args = append(args, "--syft-source-output", buildParams.SyftSourceOutput)
	}
	if buildParams.SyftImageOutput != "" {
		args = append(args, "--syft-image-output", buildParams.SyftImageOutput)
	}
	if buildParams.SBOMFormat != "" {
		args = append(args, "--sbom-format", buildParams.SBOMFormat)
	}
	if len(buildParams.ExtraArgs) > 0 {
		args = append(args, "--")
		args = append(args, buildParams.ExtraArgs...)
	}

	return container.ExecuteCommandWithOutput(KonfluxBuildCli, args...)
}

// Creates a temporary directory for the test and registers cleanup.
func setupTestContext(t *testing.T) string {
	contextDir, err := CreateTempDir("build-test-context-")
	if err != nil {
		t.Fatalf("Failed to create test context: %v", err)
	}
	t.Cleanup(func() {
		os.RemoveAll(contextDir)
	})
	return contextDir
}

// Sets up the image registry and registers cleanup.
func setupImageRegistry(t *testing.T) ImageRegistry {
	imageRegistry := NewImageRegistry()
	err := imageRegistry.Prepare()
	Expect(err).ToNot(HaveOccurred())
	err = imageRegistry.Start()
	Expect(err).ToNot(HaveOccurred())
	t.Cleanup(func() {
		imageRegistry.Stop()
	})
	return imageRegistry
}

// Writes a Containerfile with the given content to the context directory.
func writeContainerfile(contextDir, content string) {
	err := os.WriteFile(path.Join(contextDir, "Containerfile"), []byte(content), 0644)
	Expect(err).ToNot(HaveOccurred())
}

type ociHistory struct {
	CreatedBy string `json:"created_by"`
}

type containerImageMeta struct {
	digest      string
	created     string
	labels      map[string]string
	annotations map[string]string
	envs        map[string]string
	layer_ids   []string
	history     []ociHistory
}

func getImageMeta(container *TestRunnerContainer, imageRef string) containerImageMeta {
	stdout, _, err := container.ExecuteCommandWithOutput("buildah", "inspect", imageRef)
	Expect(err).ToNot(HaveOccurred())

	var inspect struct {
		OCIv1 struct {
			Created string `json:"created"`
			Config  struct {
				Labels map[string]string
				Env    []string
			} `json:"config"`
			Rootfs struct {
				DiffIDs []string `json:"diff_ids"`
			} `json:"rootfs"`
			History []ociHistory `json:"history"`
		}
		ImageAnnotations map[string]string
		// This field has the same value as the output of
		// skopeo inspect --format '{{.Digest}}' containers-storage:<imageRef>
		FromImageDigest string
	}

	err = json.Unmarshal([]byte(stdout), &inspect)
	Expect(err).ToNot(HaveOccurred())

	envs := make(map[string]string, len(inspect.OCIv1.Config.Env))
	for _, env := range inspect.OCIv1.Config.Env {
		key, value, _ := strings.Cut(env, "=")
		envs[key] = value
	}

	return containerImageMeta{
		digest:      inspect.FromImageDigest,
		created:     inspect.OCIv1.Created,
		labels:      inspect.OCIv1.Config.Labels,
		annotations: inspect.ImageAnnotations,
		envs:        envs,
		layer_ids:   inspect.OCIv1.Rootfs.DiffIDs,
		history:     inspect.OCIv1.History,
	}
}

func getContainerfileMeta(container *TestRunnerContainer, containerfileJsonPath string) containerImageMeta {
	containerfileJSON, err := container.GetFileContent(containerfileJsonPath)
	Expect(err).ToNot(HaveOccurred())

	// Unmarshal into a generic structure.
	// Can't unmarshal into the Dockerfile struct from dockerfile-json, because some of the fields
	// are of type instruction.Command (from buildkit), which is an interface type and can't be
	// unmarshalled without a custom UnmarshalJSON method.
	var containerfile struct {
		Stages []struct {
			Commands []struct {
				Name   string
				Labels []struct {
					Key   string
					Value string
				}
				Env []struct {
					Key   string
					Value string
				}
			}
		}
	}

	err = json.Unmarshal([]byte(containerfileJSON), &containerfile)
	Expect(err).ToNot(HaveOccurred())

	labels := make(map[string]string)
	envs := make(map[string]string)

	for _, cmd := range containerfile.Stages[0].Commands {
		for _, label := range cmd.Labels {
			labels[label.Key] = label.Value
		}
		for _, env := range cmd.Env {
			envs[env.Key] = env.Value
		}
	}

	return containerImageMeta{labels: labels, envs: envs}
}

func getLabelsFromLabelsJson(container *TestRunnerContainer, imageRef string) map[string]string {
	labelsJSON := getFileContentFromOutputImage(container, imageRef, "/usr/share/buildinfo/labels.json")

	var labels map[string]string
	err := json.Unmarshal([]byte(labelsJSON), &labels)
	Expect(err).ToNot(HaveOccurred())

	return labels
}

func getLegacyLabelsJson(container *TestRunnerContainer, imageRef string) map[string]string {
	labelsJSON := getFileContentFromOutputImage(container, imageRef, "/root/buildinfo/labels.json")

	var labels map[string]string
	err := json.Unmarshal([]byte(labelsJSON), &labels)
	Expect(err).ToNot(HaveOccurred())

	return labels
}

func formatAsKeyValuePairs(m map[string]string) []string {
	var pairs []string
	for k, v := range m {
		pairs = append(pairs, k+"="+v)
	}
	return pairs
}

func getFileContentFromOutputImage(container *TestRunnerContainer, imageRef string, filePath string) string {
	return runWithMountedOutputImage(container, imageRef, `cat "$CONTAINER_ROOT"/`+filePath)
}

func statFileInOutputImage(container *TestRunnerContainer, imageRef string, filePath string) string {
	return runWithMountedOutputImage(container, imageRef, `stat "$CONTAINER_ROOT"/`+filePath)
}

func fileExistsInOutputImage(container *TestRunnerContainer, imageRef string, filePath string) bool {
	stdout := runWithMountedOutputImage(container, imageRef,
		`if [[ -e "$CONTAINER_ROOT"/`+filePath+" ]]; then echo exists; else echo does not exist; fi")

	return strings.TrimSpace(stdout) == "exists"
}

// Mount the filesystem of the 'imageRef' container, set CONTAINER_ROOT=<root of mount point>
// and execute the 'script'.
//
// This approach uses 'buildah unshare --mount' to make it possible to do assertions about the
// content of the built image.
//
// Other possible approaches that didn't work:
// - Use 'buildah run $containerID ...' (most tests are FROM scratch, there are no executables to run)
// - Use 'buildah mount' directly (failed with "Error: overlay: failed to make mount private ... operation not permitted")
func runWithMountedOutputImage(container *TestRunnerContainer, imageRef string, script string) string {
	err := container.ExecuteCommand("buildah", "from", "--name=testcontainer", imageRef)
	Expect(err).ToNot(HaveOccurred())
	defer container.ExecuteCommand("buildah", "rm", "testcontainer")

	stdout, _, err := container.ExecuteCommandWithOutput(
		"buildah", "unshare", "--mount", "CONTAINER_ROOT=testcontainer", "--", "bash", "-c", script,
	)
	Expect(err).ToNot(HaveOccurred())

	return stdout
}

// By default, Expect(m1).To(Equal(m2)) is very hard to process on failure,
// because the maps are printed in a random order and the error doesn't specify
// which keys differ.
// Format the maps as key value pairs, sort them and use ConsistOf instead of Equal,
// which shows the differing key value pairs.
func expectEqualMaps(m1 map[string]string, m2 map[string]string, description ...any) {
	m1pairs := formatAsKeyValuePairs(m1)
	slices.Sort(m1pairs)

	m2pairs := formatAsKeyValuePairs(m2)
	slices.Sort(m2pairs)

	Expect(m1pairs).To(ConsistOf(m2pairs), description...)
}

// Starts a minimal HTTP forward proxy that handles CONNECT tunneling.
// The proxy runs on 127.0.0.1:<randomly selected port>.
// The test container can connect to it because it runs with --network=host.
//
// Returns the proxy address and a map of hosts that received CONNECT requests.
func startForwardProxy(t *testing.T) (string, *sync.Map) {
	var connectedHosts sync.Map

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "only CONNECT is supported", http.StatusMethodNotAllowed)
			return
		}

		connectedHosts.Store(r.Host, true)

		dest, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer dest.Close()

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacking not supported", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)

		client, _, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer client.Close()

		var wg sync.WaitGroup
		wg.Go(func() {
			io.Copy(dest, client)
			dest.Close()
		})
		wg.Go(func() {
			io.Copy(client, dest)
			client.Close()
		})
		wg.Wait()
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).ToNot(HaveOccurred())

	server := &http.Server{Handler: handler}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })

	return listener.Addr().String(), &connectedHosts
}

// List the files in a container layer blob (tar archive).
// If the blob isn't a valid tar archive, assume it's not a layer and return (nil, false).
// Else return (files, true).
func listLayerFiles(blobPath string) ([]string, bool) {
	var files []string

	f, err := os.Open(blobPath)
	Expect(err).ToNot(HaveOccurred())
	defer f.Close()

	var tarReader *tar.Reader

	gzipReader, err := gzip.NewReader(f)
	if err == nil {
		tarReader = tar.NewReader(gzipReader)
		defer gzipReader.Close()
	} else if errors.Is(err, gzip.ErrHeader) {
		err = nil
		_, seekErr := f.Seek(0, io.SeekStart)
		Expect(seekErr).ToNot(HaveOccurred())
		// the blob isn't gzipped, try untaring it directly
		tarReader = tar.NewReader(f)
	}
	Expect(err).ToNot(HaveOccurred())

	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, tar.ErrHeader) {
			// this is not a layer blob
			return nil, false
		}
		Expect(err).ToNot(HaveOccurred())

		files = append(files, header.Name)
	}

	return files, true
}

type spdxDoc struct {
	Packages []struct {
		Name string `json:"name"`
	} `json:"packages"`
}

func (s *spdxDoc) packageNames() []string {
	var names []string
	for _, pkg := range s.Packages {
		names = append(names, pkg.Name)
	}
	return names
}

func readSPDX(path string) spdxDoc {
	content, err := os.ReadFile(path)
	Expect(err).ToNot(HaveOccurred())

	var spdxDoc spdxDoc
	Expect(json.Unmarshal(content, &spdxDoc)).To(Succeed())

	return spdxDoc
}

type cyclonedxDoc struct {
	Components []struct {
		Name string `json:"name"`
	} `json:"components"`
}

func (c *cyclonedxDoc) componentNames() []string {
	var names []string
	for _, component := range c.Components {
		names = append(names, component.Name)
	}
	return names
}

func readCycloneDX(path string) cyclonedxDoc {
	content, err := os.ReadFile(path)
	Expect(err).ToNot(HaveOccurred())

	var cyclonedxDoc cyclonedxDoc
	Expect(json.Unmarshal(content, &cyclonedxDoc)).To(Succeed())

	return cyclonedxDoc
}

func TestBuild(t *testing.T) {
	SetupGomega(t)

	storagePath, err := createContainerStorageDir()
	t.Cleanup(func() { removeContainerStorageDir(storagePath) })
	Expect(err).ToNot(HaveOccurred())

	// Need two separate storage dirs, one for tests that run as root, one for non-root
	rootStoragePath, err := createContainerStorageDir()
	t.Cleanup(func() { removeContainerStorageDir(rootStoragePath) })
	Expect(err).ToNot(HaveOccurred())

	defaultOpts := []ContainerOption{WithUser("taskuser"), maybeMountContainerStorage(storagePath, "taskuser")}

	setupBuildContainerWithCleanup := func(
		t *testing.T, buildParams BuildParams, imageRegistry ImageRegistry, opts ...ContainerOption,
	) *TestRunnerContainer {
		opts = append(defaultOpts, opts...)
		container, err := setupBuildContainer(buildParams, imageRegistry, opts...)
		t.Cleanup(func() { container.DeleteIfExists() })
		Expect(err).ToNot(HaveOccurred())
		return container
	}

	t.Run("BuildOnly", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)
		writeContainerfile(contextDir, `
FROM scratch
LABEL test.label="build-test"
`)

		outputRef := "localhost/test-image:" + GenerateUniqueTag(t)

		buildParams := BuildParams{
			Context:   contextDir,
			OutputRef: outputRef,
			Push:      false,
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil)

		// Run build without pushing
		err := runBuild(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		// Verify the image exists in buildah's local storage
		err = container.ExecuteCommand("buildah", "images", outputRef)
		Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("Image %s should exist in local buildah storage", outputRef))
	})

	t.Run("BuildAndPush", func(t *testing.T) {
		SetupGomega(t)

		imageRegistry := setupImageRegistry(t)

		contextDir := setupTestContext(t)
		writeContainerfile(contextDir, fmt.Sprintf(`
FROM scratch
LABEL test.label="build-and-push-test"
LABEL %s="1h"
`, QuayExpiresAfterLabelName))

		imageRepoUrl := imageRegistry.GetTestNamespace() + "build-test-image"
		outputRef := imageRepoUrl + ":" + GenerateUniqueTag(t)

		buildParams := BuildParams{
			Context:   contextDir,
			OutputRef: outputRef,
			Push:      true,
		}

		container := setupBuildContainerWithCleanup(t, buildParams, imageRegistry)

		err := runBuild(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		lastColon := strings.LastIndex(outputRef, ":")
		tag := outputRef[lastColon+1:]

		tagExists, err := imageRegistry.CheckTagExistence(imageRepoUrl, tag)
		Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("failed to check for %s tag existence", tag))
		Expect(tagExists).To(BeTrue(), fmt.Sprintf("Expected %s to exist in registry", outputRef))
	})

	t.Run("WithExtraArgs", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)

		writeContainerfile(contextDir, `
FROM scratch
LABEL test.label="extra-args-test"
`)

		outputRef := "localhost/test-image-extra-args:" + GenerateUniqueTag(t)

		buildParams := BuildParams{
			Context:   contextDir,
			OutputRef: outputRef,
			Push:      false,
			ExtraArgs: []string{"--logfile", "/tmp/kbc-build.log"},
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil)

		err := runBuild(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		// Verify the image exists in buildah's local storage
		err = container.ExecuteCommand("buildah", "images", outputRef)
		Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("Image %s should exist in local buildah storage", outputRef))

		// Verify that the logfile was created
		err = container.ExecuteCommand("test", "-f", "/tmp/kbc-build.log")
		Expect(err).ToNot(HaveOccurred(), "Expected /tmp/kbc-build.log to exist")
	})

	t.Run("UsesRunInstruction", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)
		writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s
RUN echo hi
`, baseImage))

		t.Run("AsRoot", func(t *testing.T) {
			SetupGomega(t)

			outputRef := "localhost/test-use-run-instruction-root:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:   contextDir,
				OutputRef: outputRef,
				Push:      false,
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil,
				WithUser("root"), maybeMountContainerStorage(rootStoragePath, "root"))

			err = runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			err = container.ExecuteCommand("buildah", "images", outputRef)
			Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("Image %s should exist in local buildah storage", outputRef))
		})

		t.Run("AsNonRoot", func(t *testing.T) {
			SetupGomega(t)

			outputRef := "localhost/test-use-run-instruction-nonroot:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:   contextDir,
				OutputRef: outputRef,
				Push:      false,
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil,
				// This is the default, but let's be explicit
				WithUser("taskuser"))

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			err = container.ExecuteCommand("buildah", "images", outputRef)
			Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("Image %s should exist in local buildah storage", outputRef))
		})
	})

	t.Run("WithSecretDirs", func(t *testing.T) {
		SetupGomega(t)

		secretsBaseDir := t.TempDir()
		testutil.WriteFileTree(t, secretsBaseDir, map[string]string{
			"secret1/token":   "secret-token-value",
			"secret1/api-key": "secret-api-key-value",
			// secret2/password: symlink to ..data/password (similar to Kubernetes secret volumes)
			"secret2/..data/password": "secret-password-value",
		})
		err := os.Symlink("..data/password", filepath.Join(secretsBaseDir, "secret2/password"))
		Expect(err).ToNot(HaveOccurred())

		secretDirs := []string{
			// Should be accessible with IDs secret1_alias/*
			"src=/secrets/secret1,name=secret1_alias",
			// Should be accessible with IDs secret2/*
			"/secrets/secret2",
			// Should be ignored
			"src=/secrets/nonexistent,optional=true",
		}

		contextDir := setupTestContext(t)
		writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s

# Test that secrets are accessible during build
RUN --mount=type=secret,id=secret1_alias/token \
    --mount=type=secret,id=secret1_alias/api-key \
    --mount=type=secret,id=secret2/password \
    echo "token=$(cat /run/secrets/secret1_alias/token)" && \
    echo "api-key=$(cat /run/secrets/secret1_alias/api-key)" && \
    echo "password=$(cat /run/secrets/secret2/password)"

LABEL test.label="secret-dirs-test"
	`, baseImage))

		outputRef := "localhost/test-image-secrets:" + GenerateUniqueTag(t)

		buildParams := BuildParams{
			Context:    contextDir,
			OutputRef:  outputRef,
			Push:       false,
			SecretDirs: secretDirs,
		}

		// Setup container with extra volume for secrets
		container := setupBuildContainerWithCleanup(
			t, buildParams, nil, WithVolumeWithOptions(secretsBaseDir, "/secrets", "z"),
		)

		_, stderr, err := runBuildWithOutput(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		// Verify that the secret values appear in the build output (stderr contains build logs)
		Expect(stderr).To(ContainSubstring("token=secret-token-value"))
		Expect(stderr).To(ContainSubstring("api-key=secret-api-key-value"))
		Expect(stderr).To(ContainSubstring("password=secret-password-value"))

		// Verify the image exists in buildah's local storage
		err = container.ExecuteCommand("buildah", "images", outputRef)
		Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("Image %s should exist in local buildah storage", outputRef))
	})

	t.Run("WorkdirMount", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)

		outputRef := "localhost/test-image-workdir-mount:" + GenerateUniqueTag(t)

		buildParams := BuildParams{
			Context:      contextDir,
			OutputRef:    outputRef,
			Push:         false,
			WorkdirMount: "/buildcontext",
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil)

		err := container.ExecuteCommand("buildah", "pull", baseImage)
		Expect(err).ToNot(HaveOccurred())
		// Push the baseimage to <contextDir>/baseimage.tar (contextDir is mounted at /workspace in the container)
		err = container.ExecuteCommand(
			"buildah", "push", "--remove-signatures", baseImage, "oci-archive:/workspace/baseimage.tar",
		)
		Expect(err).ToNot(HaveOccurred())

		writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s AS builder

# /buildcontext is mounted with --volume <contextDir>:/buildcontext
# baseimage.tar exists prior to starting the build
RUN cp /buildcontext/baseimage.tar /buildcontext/myimage.tar


# This form of FROM instructions uses the filesystem of the host, not the container,
# so /buildcontext does not exist. But during the build, the context directory is
# the working directory, so this works.
FROM oci-archive:./myimage.tar

# Need to reference builder here to force ordering, otherwise buildah would skip
# the builder stage entirely.
RUN --mount=type=bind,from=builder,src=.,target=/var/tmp \
    rm /buildcontext/myimage.tar
`, baseImage))

		err = runBuild(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		// Verify the image exists in buildah's local storage
		err = container.ExecuteCommand("buildah", "images", outputRef)
		Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("Image %s should exist in local buildah storage", outputRef))
	})

	t.Run("WithBuildArgs", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)

		writeContainerfile(contextDir, `
FROM scratch

ARG NAME
ARG VERSION
ARG AUTHOR
ARG VENDOR
ARG WITH_DEFAULT=default
ARG UNDEFINED

LABEL name=$NAME
LABEL version=$VERSION
LABEL author=$AUTHOR
LABEL vendor=$VENDOR
LABEL buildarg-with-default=$WITH_DEFAULT
LABEL undefined-buildarg=$UNDEFINED

LABEL test.label="build-args-test"
`)

		testutil.WriteFileTree(t, contextDir, map[string]string{
			"build-args-file": "AUTHOR=John Doe\nVENDOR=konflux-ci.dev",
		})

		outputRef := "localhost/test-image-build-args:" + GenerateUniqueTag(t)
		// Also verify that build args are handled properly for Containerfile parsing
		containerfileJsonPath := "/workspace/parsed-containerfile.json"

		buildParams := BuildParams{
			Context:                 contextDir,
			OutputRef:               outputRef,
			Push:                    false,
			BuildArgs:               []string{"NAME=foo", "VERSION=1.2.3"},
			BuildArgsFile:           "/workspace/build-args-file",
			ContainerfileJsonOutput: containerfileJsonPath,
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil)

		err := runBuild(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		expectedLabels := []string{
			"name=foo",
			"version=1.2.3",
			"author=John Doe",
			"vendor=konflux-ci.dev",
			"buildarg-with-default=default",
			"undefined-buildarg=",
		}

		imageMeta := getImageMeta(container, outputRef)
		containerfileMeta := getContainerfileMeta(container, containerfileJsonPath)

		// Verify image labels
		imageLabels := formatAsKeyValuePairs(imageMeta.labels)
		Expect(imageLabels).To(ContainElements(expectedLabels))

		// Verify the parsed Containerfile has the same label values
		containerfileLabels := formatAsKeyValuePairs(containerfileMeta.labels)
		Expect(containerfileLabels).To(ContainElements(expectedLabels))

		// Verify that /usr/share/buildinfo/labels.json also has the same label values
		buildInfoLabels := formatAsKeyValuePairs(getLabelsFromLabelsJson(container, outputRef))
		Expect(buildInfoLabels).To(ContainElements(expectedLabels))
	})

	t.Run("PlatformBuildArgs", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)

		writeContainerfile(contextDir, `
FROM scratch

ARG TARGETPLATFORM
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG BUILDPLATFORM
ARG BUILDOS
ARG BUILDARCH
ARG BUILDVARIANT

LABEL TARGETPLATFORM=$TARGETPLATFORM
LABEL TARGETOS=$TARGETOS
LABEL TARGETARCH=$TARGETARCH
LABEL TARGETVARIANT=$TARGETVARIANT
LABEL BUILDPLATFORM=$BUILDPLATFORM
LABEL BUILDOS=$BUILDOS
LABEL BUILDARCH=$BUILDARCH
LABEL BUILDVARIANT=$BUILDVARIANT

LABEL test.label="platform-build-args-test"
`)

		outputRef := "localhost/test-image-platform-build-args:" + GenerateUniqueTag(t)
		containerfileJsonPath := "/workspace/parsed-containerfile.json"

		buildParams := BuildParams{
			Context:                 contextDir,
			OutputRef:               outputRef,
			Push:                    false,
			ContainerfileJsonOutput: containerfileJsonPath,
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil)

		err := runBuild(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		// Verify platform values match between the parsed Containerfile, the actual image and buildinfo
		imageLabels := getImageMeta(container, outputRef).labels
		containerfileLabels := getContainerfileMeta(container, containerfileJsonPath).labels
		buildinfoLabels := getLabelsFromLabelsJson(container, outputRef)

		labelsToCheck := []string{
			"TARGETPLATFORM",
			"TARGETOS",
			"TARGETARCH",
			"TARGETVARIANT",
			"BUILDPLATFORM",
			"BUILDOS",
			"BUILDARCH",
			"BUILDVARIANT",
		}

		for _, label := range labelsToCheck {
			imageLabel := imageLabels[label]
			containerfileLabel := containerfileLabels[label]
			buildinfoLabel := buildinfoLabels[label]

			Expect(imageLabel).To(Equal(containerfileLabel),
				fmt.Sprintf("image label: %s=%s; containerfile label: %s=%s", label, imageLabel, label, containerfileLabel),
			)
			Expect(imageLabel).To(Equal(buildinfoLabel),
				fmt.Sprintf("image label: %s=%s; buildinfo label: %s=%s", label, imageLabel, label, buildinfoLabel),
			)
		}
	})

	t.Run("ContainerfileJsonOutput", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)

		writeContainerfile(contextDir, `FROM scratch`)

		outputRef := "localhost/test-containerfile-json-output:" + GenerateUniqueTag(t)
		containerfileJsonPath := "/workspace/parsed-containerfile.json"

		buildParams := BuildParams{
			Context:                 contextDir,
			OutputRef:               outputRef,
			Push:                    false,
			ContainerfileJsonOutput: containerfileJsonPath,
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil)

		err := runBuild(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		containerfileJSON, err := container.GetFileContent(containerfileJsonPath)
		Expect(err).ToNot(HaveOccurred())

		Expect(containerfileJSON).To(Equal(`{
  "MetaArgs": null,
  "Stages": [
    {
      "Name": "",
      "OrigCmd": "FROM",
      "BaseName": "scratch",
      "Platform": "",
      "Comment": "",
      "SourceCode": "FROM scratch",
      "Location": [
        {
          "Start": {
            "Line": 1,
            "Character": 0
          },
          "End": {
            "Line": 1,
            "Character": 0
          }
        }
      ],
      "From": {
        "Scratch": true
      },
      "Commands": null
    }
  ]
}`))
	})

	t.Run("WithEnvs", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)

		writeContainerfile(contextDir, `
FROM scratch

LABEL foo=$FOO
LABEL bar=$BAR

LABEL test.label="envs-test"
`)

		outputRef := "localhost/test-image-envs:" + GenerateUniqueTag(t)
		// Also verify that envs are handled properly for Containerfile parsing
		containerfileJsonPath := "/workspace/parsed-containerfile.json"

		buildParams := BuildParams{
			Context:   contextDir,
			OutputRef: outputRef,
			Push:      false,
			Envs: []string{
				"FOO=foo-value",
				"BAR=bar-value",
				// Corner cases to verify that dockerfile-json and buildah handle them the same way
				// Should be an env var without a name (causes an error when starting the container)
				"=noname",
				// Should be an empty string
				"NOVALUE=",
				// Shouldn't be set at all
				"NOSUCHENV",
			},
			ContainerfileJsonOutput: containerfileJsonPath,
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil)

		err := runBuild(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		expectedEnvs := []string{
			"FOO=foo-value",
			"BAR=bar-value",
			"=noname",
			"NOVALUE=",
		}
		expectedLabels := []string{
			"foo=foo-value",
			"bar=bar-value",
		}

		imageMeta := getImageMeta(container, outputRef)
		containerfileMeta := getContainerfileMeta(container, containerfileJsonPath)

		// Verify envs
		imageEnvs := formatAsKeyValuePairs(imageMeta.envs)
		Expect(imageEnvs).To(ContainElements(expectedEnvs))

		containerfileEnvs := formatAsKeyValuePairs(containerfileMeta.envs)
		Expect(containerfileEnvs).To(ContainElements(expectedEnvs))

		Expect(imageMeta.envs).ToNot(HaveKey("NOSUCHENV"))
		Expect(containerfileMeta.envs).ToNot(HaveKey("NOSUCHENV"))

		// Verify labels
		imageLabels := formatAsKeyValuePairs(imageMeta.labels)
		Expect(imageLabels).To(ContainElements(expectedLabels))

		containerfileLabels := formatAsKeyValuePairs(containerfileMeta.labels)
		Expect(containerfileLabels).To(ContainElements(expectedLabels))

		buildinfoLabels := formatAsKeyValuePairs(getLabelsFromLabelsJson(container, outputRef))
		Expect(buildinfoLabels).To(ContainElements(expectedLabels))
	})

	t.Run("WithLabelsAndAnnotations", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)

		writeContainerfile(contextDir, `FROM scratch`)

		outputRef := "localhost/test-image-labels-annotations:" + GenerateUniqueTag(t)

		buildParams := BuildParams{
			Context:   contextDir,
			OutputRef: outputRef,
			Push:      false,
			Labels: []string{
				"custom.label1=value1",
				"custom.label2=value2",
			},
			Annotations: []string{
				"org.opencontainers.image.title=King Arthur",
				"org.opencontainers.image.description=Elected by farcical aquatic ceremony.",
			},
			ImageSource:           "https://github.com/konflux-ci/test",
			ImageRevision:         "abc123",
			LegacyBuildTimestamp:  "1767225600", // 2026-01-01
			QuayImageExpiresAfter: "2w",
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil)

		err := runBuild(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		expectedLabels := []string{
			"custom.label1=value1",
			"custom.label2=value2",
			// default labels (with user-supplied values)
			"org.opencontainers.image.source=https://github.com/konflux-ci/test",
			"org.opencontainers.image.revision=abc123",
			"org.opencontainers.image.created=2026-01-01T00:00:00Z",
			"quay.expires-after=2w",
		}

		expectedAnnotations := []string{
			"org.opencontainers.image.title=King Arthur",
			"org.opencontainers.image.description=Elected by farcical aquatic ceremony.",
			// default annotations (with user-supplied values)
			"org.opencontainers.image.source=https://github.com/konflux-ci/test",
			"org.opencontainers.image.revision=abc123",
			"org.opencontainers.image.created=2026-01-01T00:00:00Z",
		}

		imageMeta := getImageMeta(container, outputRef)

		imageLabels := formatAsKeyValuePairs(imageMeta.labels)
		Expect(imageLabels).To(ContainElements(expectedLabels))

		buildinfoLabels := formatAsKeyValuePairs(getLabelsFromLabelsJson(container, outputRef))
		Expect(buildinfoLabels).To(ContainElements(expectedLabels))

		imageAnnotations := formatAsKeyValuePairs(imageMeta.annotations)
		Expect(imageAnnotations).To(ContainElements(expectedAnnotations))
	})

	t.Run("AnnotationsFile", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)

		writeContainerfile(contextDir, `FROM scratch`)

		annotationsFileContent := `
# overrides default annotation
org.opencontainers.image.created=never

annotation.from.file=overridden-below
annotation.from.file=annotation-from-file

common.annotation=overridden-by-cli-annotation
`
		testutil.WriteFileTree(t, contextDir, map[string]string{
			"annotations.cfg": annotationsFileContent,
		})

		outputRef := "localhost/test-annotations-file:" + GenerateUniqueTag(t)

		buildParams := BuildParams{
			Context:   contextDir,
			OutputRef: outputRef,
			Push:      false,
			Annotations: []string{
				"annotation.from.cli=annotation-from-CLI",
				"common.annotation=common-annotation-from-CLI",
			},
			AnnotationsFile: "/workspace/annotations.cfg",
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil)

		err := runBuild(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		imageMeta := getImageMeta(container, outputRef)

		imageAnnotations := formatAsKeyValuePairs(imageMeta.annotations)
		Expect(imageAnnotations).To(ContainElements(
			"org.opencontainers.image.created=never",
			"annotation.from.file=annotation-from-file",
			"annotation.from.cli=annotation-from-CLI",
			"common.annotation=common-annotation-from-CLI",
		))
	})

	t.Run("OverrideDefaultLabelsAndAnnotations", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)

		writeContainerfile(contextDir, `FROM scratch`)

		outputRef := "localhost/test-override-defaults:" + GenerateUniqueTag(t)

		buildParams := BuildParams{
			Context:       contextDir,
			OutputRef:     outputRef,
			Push:          false,
			ImageSource:   "https://default.com",
			ImageRevision: "default",
			Labels: []string{
				// override source, but not revision
				"org.opencontainers.image.source=https://user-override.com",
			},
			Annotations: []string{
				// override revision, but not source
				"org.opencontainers.image.revision=override",
			},
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil)

		err := runBuild(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		imageMeta := getImageMeta(container, outputRef)

		imageLabels := formatAsKeyValuePairs(imageMeta.labels)
		Expect(imageLabels).To(ContainElements(
			"org.opencontainers.image.source=https://user-override.com",
			"org.opencontainers.image.revision=default",
		))
		buildinfoLabels := formatAsKeyValuePairs(getLabelsFromLabelsJson(container, outputRef))
		Expect(buildinfoLabels).To(ContainElements(
			"org.opencontainers.image.source=https://user-override.com",
			"org.opencontainers.image.revision=default",
		))

		imageAnnotations := formatAsKeyValuePairs(imageMeta.annotations)
		Expect(imageAnnotations).To(ContainElements(
			"org.opencontainers.image.source=https://default.com",
			"org.opencontainers.image.revision=override",
		))
	})

	t.Run("WithLegacyLabels", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)

		writeContainerfile(contextDir, `FROM scratch`)

		outputRef := "localhost/test-legacy-labels:" + GenerateUniqueTag(t)

		buildParams := BuildParams{
			Context:              contextDir,
			OutputRef:            outputRef,
			Push:                 false,
			ImageSource:          "https://github.com/konflux-ci/test",
			ImageRevision:        "abc123",
			LegacyBuildTimestamp: "1767225600", // 2026-01-01
			AddLegacyLabels:      true,
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil)

		err := runBuild(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		imageMeta := getImageMeta(container, outputRef)
		buildinfoLabels := getLabelsFromLabelsJson(container, outputRef)

		// Get the expected architecture from 'uname -m' in the container
		stdout, _, err := container.ExecuteCommandWithOutput("uname", "-m")
		Expect(err).ToNot(HaveOccurred())
		expectedArch := strings.TrimSpace(stdout)

		expectedLabels := []string{
			"org.opencontainers.image.source=https://github.com/konflux-ci/test",
			"vcs-url=https://github.com/konflux-ci/test",
			"org.opencontainers.image.revision=abc123",
			"vcs-ref=abc123",
			"vcs-type=git",
			"org.opencontainers.image.created=2026-01-01T00:00:00Z",
			"build-date=2026-01-01T00:00:00Z",
		}

		imageLabels := formatAsKeyValuePairs(imageMeta.labels)
		buildinfoLabelPairs := formatAsKeyValuePairs(buildinfoLabels)

		Expect(imageMeta.labels).To(HaveKey("architecture"))
		Expect(imageMeta.labels["architecture"]).To(Equal(expectedArch),
			"architecture label should match uname -m output")
		Expect(buildinfoLabels).To(HaveKey("architecture"))
		Expect(buildinfoLabels["architecture"]).To(Equal(expectedArch),
			"architecture label should match uname -m output")

		Expect(imageLabels).To(ContainElements(expectedLabels))
		Expect(buildinfoLabelPairs).To(ContainElements(expectedLabels))

		// Annotations should not include legacy labels
		imageAnnotations := formatAsKeyValuePairs(imageMeta.annotations)
		Expect(imageMeta.annotations).ToNot(HaveKey("architecture"))
		Expect(imageMeta.annotations).ToNot(HaveKey("vcs-url"))
		Expect(imageMeta.annotations).ToNot(HaveKey("vcs-ref"))
		Expect(imageMeta.annotations).ToNot(HaveKey("vcs-type"))
		Expect(imageMeta.annotations).ToNot(HaveKey("build-date"))
		Expect(imageAnnotations).To(ContainElements(
			"org.opencontainers.image.source=https://github.com/konflux-ci/test",
			"org.opencontainers.image.revision=abc123",
			"org.opencontainers.image.created=2026-01-01T00:00:00Z",
		))
	})

	t.Run("SourceDateEpoch", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)

		writeContainerfile(contextDir, `
FROM scratch

ARG SOURCE_DATE_EPOCH

LABEL source-date-epoch=$SOURCE_DATE_EPOCH
`)

		sourceDateEpoch := "1767225600" // 2026-01-01

		checkSourceDateEpochEffects := func(imageMeta containerImageMeta) {
			imageAnnotations := formatAsKeyValuePairs(imageMeta.annotations)
			imageLabels := formatAsKeyValuePairs(imageMeta.labels)

			// Should set the 'created' attribute
			Expect(imageMeta.created).To(Equal("2026-01-01T00:00:00Z"))
			Expect(imageAnnotations).To(ContainElements(
				// Should set the org.opencontainers.image.created annotation
				"org.opencontainers.image.created=2026-01-01T00:00:00Z",
			))
			Expect(imageLabels).To(ContainElements(
				// Should set the org.opencontainers.image.created label
				"org.opencontainers.image.created=2026-01-01T00:00:00Z",
				// With --add-legacy-labels, should also set the build-date label
				"build-date=2026-01-01T00:00:00Z",
				// Should set the SOURCE_DATE_EPOCH build argument
				"source-date-epoch=1767225600",
			))
		}

		t.Run("FromCLI", func(t *testing.T) {
			SetupGomega(t)

			outputRef := "localhost/test-source-date-epoch-from-cli:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:         contextDir,
				OutputRef:       outputRef,
				Push:            false,
				SourceDateEpoch: sourceDateEpoch,
				AddLegacyLabels: true,
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil)

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			imageMeta := getImageMeta(container, outputRef)
			checkSourceDateEpochEffects(imageMeta)
		})

		// Test the SOURCE_DATE_EPOCH environment variable as well, because unlike other env vars,
		// it doesn't have the KBC_ prefix and we still want to handle it.
		t.Run("FromEnv", func(t *testing.T) {
			SetupGomega(t)

			outputRef := "localhost/test-source-date-epoch-from-cli:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:         contextDir,
				OutputRef:       outputRef,
				Push:            false,
				AddLegacyLabels: true,
			}

			container := setupBuildContainerWithCleanup(
				t, buildParams, nil, WithEnv("SOURCE_DATE_EPOCH", sourceDateEpoch),
			)

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			imageMeta := getImageMeta(container, outputRef)
			checkSourceDateEpochEffects(imageMeta)
		})
	})

	t.Run("Reproducibility", func(t *testing.T) {
		SetupGomega(t)

		buildImage := func() containerImageMeta {
			contextDir := setupTestContext(t)
			// The file is newly created for every test build, has a different timestamp every time
			// and would normally break reproducibility. Try setting RewriteTimestamp to false and
			// see that imageMeta.digest is different every time.
			testutil.WriteFileTree(t, contextDir, map[string]string{
				"hello.txt": "hello there\n",
			})

			writeContainerfile(contextDir, `
FROM scratch

COPY hello.txt /hello.txt
`)

			outputRef := "localhost/test-reproducibility:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:   contextDir,
				OutputRef: outputRef,
				Push:      false,
				// Thanks to the combination of --source-date-epoch and --rewrite-timestamp,
				// the timestamp of /hello.txt inside the built image will be clamped to 2026-01-01
				SourceDateEpoch:  "1767225600",
				RewriteTimestamp: true,
				// Add more labels to test that injecting labels.json doesn't break reproducibility.
				// E.g. if the order of labels in the file was random, it *would* break reproducibility.
				AddLegacyLabels: true,
				Labels:          []string{"label1=foo", "label2=bar"},
				// We want to build the image twice and verify reproducibility, avoid caching
				NoCache: true,
				ExtraArgs: []string{
					// Ensure the image config will be exactly the same regardles of the host OS/architecture
					"--platform", "linux/amd64",
				},
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil)

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			imageMeta := getImageMeta(container, outputRef)
			return imageMeta
		}

		imageMeta1 := buildImage()

		// Thanks to all time-related metadata being set to 2026-01-01, the build is fully reproducible
		// and the digest will stay the same every time.
		Expect(imageMeta1.digest).To(Equal("sha256:678998fc429d59022f0966551854f99bd1c553c01b74a093a420a927576f5d8a"))
		Expect(imageMeta1.created).To(Equal("2026-01-01T00:00:00Z"))

		// Ensure the hello.txt file created for the second test really does get a different timestamp
		time.Sleep(1 * time.Second)

		// Build from the same inputs (just with different timestamps) twice.
		// The built image should have the same digest both times.
		imageMeta2 := buildImage()
		Expect(imageMeta2.digest).To(Equal(imageMeta1.digest), "Digest unexpectedly changed after rebuild")
	})

	t.Run("InjectingBuildinfo", func(t *testing.T) {
		SetupGomega(t)

		setupPrefetchDirWithSbom := func(t *testing.T) string {
			prefetchDir := t.TempDir()
			// Needs group-write permissions so that taskuser can write to it
			Expect(os.Chmod(prefetchDir, 0775)).To(Succeed())
			Expect(os.Mkdir(filepath.Join(prefetchDir, "output"), 0755)).To(Succeed())

			sbomContent := `{
				"bomFormat": "CycloneDX",
				"components": [
					{"purl": "pkg:rpm/redhat/bash@5.1.8?repository_id=ubi-10-baseos-rpms"},
					{"purl": "pkg:rpm/redhat/glibc@2.34?repository_id=ubi-10-appstream-rpms"}
				]
			}`
			sbomPath := filepath.Join(prefetchDir, "output", "bom.json")
			Expect(os.WriteFile(sbomPath, []byte(sbomContent), 0644)).To(Succeed())

			return prefetchDir
		}

		t.Run("LabelsJSON", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)

			writeContainerfile(contextDir, fmt.Sprintf(`
# Real base image with its own labels, such as:
# - com.redhat.component
# - name
# - vendor
FROM %s

# Built-in buildarg
ARG BUILDOS
LABEL build.os=$BUILDOS

# User-provided arg
ARG FOO
LABEL buildarg.label=$FOO

# User-provided env
LABEL env.label=$BAR

# Static value
LABEL static.label=static-value

# Same as a regular buildah build, the number of layers in the resulting image
# should be [number of layers in the base image] + 1. Injecting the labels.json
# file must not affect that. To verify this, add more instructions that create
# layers, otherwise the injected COPY instruction could be the only one.
RUN echo "this instruction creates an intermediate layer" > /tmp/foo.txt
RUN echo "this instruction also creates an intermediate layer" > /tmp/bar.txt
`, baseImage))

			outputRef := "localhost/test-injecting-buildinfo-all-label-sources:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:   contextDir,
				OutputRef: outputRef,
				Push:      false,
				// Set a conspicuous source date epoch so that we can more easily verify
				// that our 'created' label overrides the one from the base image
				SourceDateEpoch: "882921600", // 1997-12-24
				BuildArgs:       []string{"FOO=foo"},
				Envs:            []string{"BAR=bar"},
				Labels:          []string{"cli.label=value-gets-overridden", "cli.label=label-from-CLI"},
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil)

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			injectedLabels := getLabelsFromLabelsJson(container, outputRef)
			Expect(injectedLabels).To(SatisfyAll(
				// Base image labels
				HaveKeyWithValue("com.redhat.component", "ubi10-micro-container"),
				HaveKeyWithValue("name", "ubi10/ubi-micro"),
				HaveKeyWithValue("vendor", "Red Hat, Inc."),
				// Normally auto-injected, but not if --timestamp or --source-date-epoch is used
				// (see https://www.mankier.com/1/buildah-build#--identity-label).
				// So in this case, it comes from the base image.
				HaveKeyWithValue("io.buildah.version", "1.41.4"),
				// Containerfile labels
				HaveKeyWithValue("build.os", "linux"),
				HaveKeyWithValue("buildarg.label", "foo"),
				HaveKeyWithValue("env.label", "bar"),
				HaveKeyWithValue("static.label", "static-value"),
				// Auto-injected labels
				HaveKeyWithValue("org.opencontainers.image.created", "1997-12-24T00:00:00Z"),
				// CLI labels
				HaveKeyWithValue("cli.label", "label-from-CLI"),
			))

			imageMeta := getImageMeta(container, outputRef)
			expectEqualMaps(injectedLabels, imageMeta.labels,
				"Expected labels.json (top) to match the actual image labels (bottom)")

			Expect(imageMeta.layer_ids).To(HaveLen(2),
				"Expected the output image to have 2 layers: [number of base image layers] + 1",
			)

			stat := statFileInOutputImage(container, outputRef, "/usr/share/buildinfo/labels.json")
			Expect(stat).To(ContainSubstring("Access: (0644/-rw-r--r--)"),
				"The injected labels.json file should have mode 0644")

			// The image WILL have /root/buildinfo/labels.json even though we don't inject it by default.
			// It comes from the base image, which already has the file.
			legacyLabels := getLegacyLabelsJson(container, outputRef)
			Expect(legacyLabels).To(SatisfyAll(
				// Has base image labels
				HaveKeyWithValue("com.redhat.component", "ubi10-micro-container"),
				HaveKeyWithValue("name", "ubi10/ubi-micro"),
				HaveKeyWithValue("vendor", "Red Hat, Inc."),
				// But not any of the new labels
				Not(HaveKey("build.os")),
				Not(HaveKey("buildarg.label")),
				Not(HaveKey("env.label")),
				Not(HaveKey("static.label")),
				Not(HaveKey("cli.label")),
			))
		})

		t.Run("LabelsFromEarlierStages", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)

			writeContainerfile(contextDir, fmt.Sprintf(`
# This stage is unused, because the second stage1 overrides it
# ...but only sometimes. It appears buildah's behavior is not deterministic.
# Dropping the first stage1 until https://github.com/containers/buildah/issues/6731
# is fixed and we get an updated version of buildah.
# (We'll know because the PrePullImages tests will start skipping the unused stage.)
#FROM scratch AS stage1
#
#LABEL stage1.label=this-stage-is-unused
#LABEL common.build.label=this-stage-is-unused
#LABEL common.label=this-stage-is-unused


# Real base image
FROM %s AS stage1

LABEL stage1.label=value-gets-overridden
LABEL stage1.label=label-from-stage1

LABEL common.build.label=overridden-in-stage-2
LABEL common.label=overridden-in-final-stage


FROM stage1 AS stage2

LABEL stage2.label=value-gets-overridden
LABEL stage2.label=label-from-stage2

LABEL common.build.label=common-build-stage2


# Not a dependency for the final stage, buildah will skip this completely
FROM base.image.does.not/exist:latest AS unused-stage

LABEL unused.stage.label=label-from-unused-stage


FROM stage2

LABEL final.stage.label=value-gets-overridden
LABEL final.stage.label=label-from-final-stage

LABEL common.label=common-final-stage
`, baseImage))

			outputRef := "localhost/test-injecting-buildinfo-labels-from-earlier-stages:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:   contextDir,
				OutputRef: outputRef,
				Push:      false,
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil)

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			injectedLabels := getLabelsFromLabelsJson(container, outputRef)
			Expect(injectedLabels).To(SatisfyAll(
				// Base image labels
				HaveKeyWithValue("com.redhat.component", "ubi10-micro-container"),
				HaveKeyWithValue("name", "ubi10/ubi-micro"),
				HaveKeyWithValue("vendor", "Red Hat, Inc."),
				// Labels from stages
				HaveKeyWithValue("stage1.label", "label-from-stage1"),
				HaveKeyWithValue("stage2.label", "label-from-stage2"),
				HaveKeyWithValue("final.stage.label", "label-from-final-stage"),
				HaveKeyWithValue("common.build.label", "common-build-stage2"),
				HaveKeyWithValue("common.label", "common-final-stage"),
				// Buildah version label. We don't know the version, just check that it exists.
				// The correct value is later verified by comparing labels.json to the actual labels.
				HaveKey("io.buildah.version"),
				// Should not have label from unused stage
				Not(HaveKey("unused.stage.label")),
			))

			imageMeta := getImageMeta(container, outputRef)
			expectEqualMaps(injectedLabels, imageMeta.labels,
				"Expected labels.json (top) to match the actual image labels (bottom)")
		})

		t.Run("LabelsFromScratch", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)

			writeContainerfile(contextDir, `
FROM scratch

LABEL containerfile.label=label-from-containerfile
`)

			outputRef := "localhost/test-injecting-buildinfo-labels-from-scratch:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:   contextDir,
				OutputRef: outputRef,
				Push:      false,
				Labels:    []string{"cli.label=label-from-CLI"},
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil)

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			injectedLabels := getLabelsFromLabelsJson(container, outputRef)
			Expect(injectedLabels).To(SatisfyAll(
				HaveKey("org.opencontainers.image.created"),
				HaveKeyWithValue("containerfile.label", "label-from-containerfile"),
				HaveKeyWithValue("cli.label", "label-from-CLI"),
			))

			imageMeta := getImageMeta(container, outputRef)
			expectEqualMaps(injectedLabels, imageMeta.labels,
				"Expected labels.json (top) to match the actual image labels (bottom)")

			legacyPathExists := fileExistsInOutputImage(container, outputRef, "/root/buildinfo/labels.json")
			Expect(legacyPathExists).To(BeFalse(), "Should not have injected /root/buildinfo/labels.json by default")
		})

		t.Run("LabelsWithQuotes", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)

			writeContainerfile(contextDir, `
FROM scratch

LABEL with.single.quotes='label with single quotes'
LABEL with.double.quotes="label with double quotes"

ARG SINGLE_QUOTED_ARG='arg with single quotes'
ARG DOUBLE_QUOTED_ARG="arg with double quotes"

LABEL unquoted.arg.with.single.quotes=$SINGLE_QUOTED_ARG
LABEL unquoted.arg.with.double.quotes=$DOUBLE_QUOTED_ARG

LABEL quoted.arg.with.single.quotes="$SINGLE_QUOTED_ARG"
LABEL quoted.arg.with.double.quotes="$DOUBLE_QUOTED_ARG"

LABEL literal.argname.1='$SINGLE_QUOTED_ARG'
LABEL literal.argname.2='$DOUBLE_QUOTED_ARG'
`)

			outputRef := "localhost/test-injecting-buildinfo-labels-with-quotes:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:   contextDir,
				OutputRef: outputRef,
				Push:      false,
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil)

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			injectedLabels := getLabelsFromLabelsJson(container, outputRef)
			// LABEL commands follow shell expansion rules for quotes
			Expect(injectedLabels).To(SatisfyAll(
				HaveKeyWithValue("with.single.quotes", "label with single quotes"),
				HaveKeyWithValue("with.double.quotes", "label with double quotes"),
				HaveKeyWithValue("unquoted.arg.with.single.quotes", "arg with single quotes"),
				HaveKeyWithValue("unquoted.arg.with.double.quotes", "arg with double quotes"),
				HaveKeyWithValue("quoted.arg.with.single.quotes", "arg with single quotes"),
				HaveKeyWithValue("quoted.arg.with.double.quotes", "arg with double quotes"),
				HaveKeyWithValue("literal.argname.1", "$SINGLE_QUOTED_ARG"),
				HaveKeyWithValue("literal.argname.2", "$DOUBLE_QUOTED_ARG"),
			))

			imageMeta := getImageMeta(container, outputRef)
			expectEqualMaps(injectedLabels, imageMeta.labels,
				"Expected labels.json (top) to match the actual image labels (bottom)")
		})

		t.Run("AvoidsContainerignore", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)

			writeContainerfile(contextDir, `FROM scratch`)

			testutil.WriteFileTree(t, contextDir, map[string]string{
				// Ignore everything, but has no effect on COPY --from=<different buildcontext>
				".containerignore": "*\n.*\n",
			})

			outputRef := "localhost/test-injecting-buildinfo-force-through-containerignore:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:   contextDir,
				OutputRef: outputRef,
				Push:      false,
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil)

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			injectedLabels := getLabelsFromLabelsJson(container, outputRef)
			Expect(injectedLabels).To(HaveKey("io.buildah.version"))
			Expect(injectedLabels).To(HaveKey("org.opencontainers.image.created"))

			imageMeta := getImageMeta(container, outputRef)
			expectEqualMaps(injectedLabels, imageMeta.labels,
				"Expected labels.json (top) to match the actual image labels (bottom)")
		})

		t.Run("BuildahVersionPrecedence", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)

			writeContainerfile(contextDir, `
FROM scratch

LABEL io.buildah.version=0.0.1
`)

			outputRef := "localhost/test-injecting-buildinfo-buildah-version-precedence:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:   contextDir,
				OutputRef: outputRef,
				Push:      false,
				Labels:    []string{"io.buildah.version=0.1.0"},
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil)

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			injectedLabels := getLabelsFromLabelsJson(container, outputRef)
			Expect(injectedLabels).To(HaveKey("io.buildah.version"))
			// The auto-injected buildah version label cannot be overridden by anything
			Expect(injectedLabels["io.buildah.version"]).To(Not(HavePrefix("0")))

			imageMeta := getImageMeta(container, outputRef)
			expectEqualMaps(injectedLabels, imageMeta.labels,
				"Expected labels.json (top) to match the actual image labels (bottom)")
		})

		t.Run("NoBuildahVersion", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)

			writeContainerfile(contextDir, `FROM scratch`)

			outputRef := "localhost/test-injecting-buildinfo-no-buildah-version:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:   contextDir,
				OutputRef: outputRef,
				Push:      false,
				// --source-date-epoch disables the io.buildah.version injection
				SourceDateEpoch: "1767225600",
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil)

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			injectedLabels := getLabelsFromLabelsJson(container, outputRef)
			Expect(injectedLabels).ToNot(HaveKey("io.buildah.version"))

			imageMeta := getImageMeta(container, outputRef)
			expectEqualMaps(injectedLabels, imageMeta.labels,
				"Expected labels.json (top) to match the actual image labels (bottom)")
		})

		t.Run("ContentSetsJSON", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)

			writeContainerfile(contextDir, `FROM scratch`)
			prefetchDir := setupPrefetchDirWithSbom(t)

			outputRef := "localhost/test-injecting-buildinfo-content-sets:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:     contextDir,
				OutputRef:   outputRef,
				Push:        false,
				PrefetchDir: "/prefetch",
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil,
				WithVolumeWithOptions(prefetchDir, "/prefetch", "z"))

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			contentsSetsContent := getFileContentFromOutputImage(container, outputRef, "/usr/share/buildinfo/content-sets.json")

			var contentManifest struct {
				ContentSets []string `json:"content_sets"`
			}
			Expect(json.Unmarshal([]byte(contentsSetsContent), &contentManifest)).To(Succeed())

			Expect(contentManifest.ContentSets).To(Equal([]string{
				"ubi-10-appstream-rpms",
				"ubi-10-baseos-rpms",
			}))

			stat := statFileInOutputImage(container, outputRef, "/usr/share/buildinfo/content-sets.json")
			Expect(stat).To(ContainSubstring("Access: (0644/-rw-r--r--)"),
				"The injected content-sets.json file should have mode 0644")

			// Also verify that content-sets.json injection doesn't break labels.json injection
			labelsExist := fileExistsInOutputImage(container, outputRef, "/usr/share/buildinfo/labels.json")
			Expect(labelsExist).To(BeTrue(), "should have injected labels.json along with content-sets.json")
		})

		t.Run("SkipInjections", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)

			writeContainerfile(contextDir, `FROM scratch`)
			prefetchDir := setupPrefetchDirWithSbom(t)

			outputRef := "localhost/test-injecting-buildinfo-skip-injections:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:        contextDir,
				OutputRef:      outputRef,
				Push:           false,
				SkipInjections: true,
				PrefetchDir:    "/prefetch",
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil,
				WithVolumeWithOptions(prefetchDir, "/prefetch", "z"))

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			exists := fileExistsInOutputImage(container, outputRef, "/usr/share/buildinfo")
			Expect(exists).To(BeFalse(), "Should not have injected anything into /usr/share/buildinfo")

			exists2 := fileExistsInOutputImage(container, outputRef, "/root/buildinfo")
			Expect(exists2).To(BeFalse(), "Should not have injected anything into /root/buildinfo")
		})

		t.Run("LegacyBuildinfoPath", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)

			writeContainerfile(contextDir, `FROM scratch`)
			prefetchDir := setupPrefetchDirWithSbom(t)

			outputRef := "localhost/test-injecting-buildinfo-legacy-buildinfo-path:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:                    contextDir,
				OutputRef:                  outputRef,
				Push:                       false,
				IncludeLegacyBuildinfoPath: true,
				Labels:                     []string{"foo=bar"},
				PrefetchDir:                "/prefetch",
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil,
				WithVolumeWithOptions(prefetchDir, "/prefetch", "z"))

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			// Labels
			labelsContent := getFileContentFromOutputImage(container, outputRef, "/usr/share/buildinfo/labels.json")
			legacyLabelsContent := getFileContentFromOutputImage(container, outputRef, "/root/buildinfo/labels.json")

			// The files should be byte-for-byte equal, compare the actual content
			Expect(labelsContent).To(Equal(legacyLabelsContent),
				"/usr/share/buildinfo/labels.json (top) does not match /root/buildinfo/labels.json (bottom)")

			Expect(labelsContent).To(ContainSubstring(`"foo": "bar"`))

			stat := statFileInOutputImage(container, outputRef, "/root/buildinfo/labels.json")
			Expect(stat).To(ContainSubstring("Access: (0644/-rw-r--r--)"),
				"The injected labels.json file should have mode 0644")

			// Content sets
			contentSetsContent := getFileContentFromOutputImage(container, outputRef, "/usr/share/buildinfo/content-sets.json")
			legacyContentSetsContent := getFileContentFromOutputImage(container, outputRef, "/root/buildinfo/content-sets.json")

			Expect(contentSetsContent).To(Equal(legacyContentSetsContent),
				"/usr/share/buildinfo/content-sets.json (top) does not match /root/buildinfo/content-sets.json (bottom)")

			stat = statFileInOutputImage(container, outputRef, "/root/buildinfo/content-sets.json")
			Expect(stat).To(ContainSubstring("Access: (0644/-rw-r--r--)"),
				"The injected content-sets.json file should have mode 0644")
		})

		t.Run("KnownIssues", func(t *testing.T) {
			SetupGomega(t)

			t.Run("UnsupportedBaseImage", func(t *testing.T) {
				SetupGomega(t)

				contextDir := setupTestContext(t)

				writeContainerfile(contextDir, `
# Note that this case is technically supportable, because the ./base_image exists
# before the build starts, so we *could* inspect it.
# But for any real-world use cases, this will not be the case. The ./base_image
# will be created during the build (like in the WorkdirMount testcase).
FROM oci:./base_image

LABEL containerfile.label=containerfile-label
`)

				outputRef := "localhost/test-injecting-buildinfo-unsupported-base-image:" + GenerateUniqueTag(t)

				buildParams := BuildParams{
					Context:   contextDir,
					OutputRef: outputRef,
					Push:      false,
					Labels:    []string{"cli.label=cli-label"},
				}

				container := setupBuildContainerWithCleanup(t, buildParams, nil)

				err := container.ExecuteCommand("buildah", "pull", baseImage)
				Expect(err).ToNot(HaveOccurred())
				// Push baseimage to <contextDir>/base_image (contextDir is mounted at /workspace)
				err = container.ExecuteCommand(
					"buildah", "push", "--remove-signatures", baseImage, "oci:/workspace/base_image",
				)
				Expect(err).ToNot(HaveOccurred())

				err = runBuild(container, buildParams)
				Expect(err).ToNot(HaveOccurred())

				injectedLabels := getLabelsFromLabelsJson(container, outputRef)
				// labels.json will still have labels from Containerfile etc.
				Expect(injectedLabels).To(SatisfyAll(
					HaveKeyWithValue("containerfile.label", "containerfile-label"),
					HaveKeyWithValue("cli.label", "cli-label"),
					HaveKey("io.buildah.version"),
				))
				// labels.json will NOT have any base image labels
				Expect(injectedLabels).To(SatisfyAll(
					Not(HaveKey("com.redhat.component")),
					Not(HaveKey("name")),
					Not(HaveKey("vendor")),
				))
				// but the actual image WILL have them
				imageMeta := getImageMeta(container, outputRef)
				Expect(imageMeta.labels).To(SatisfyAll(
					HaveKey("com.redhat.component"),
					HaveKey("name"),
					HaveKey("vendor"),
				))
			})

			t.Run("UnsupportedWithTarget", func(t *testing.T) {
				SetupGomega(t)

				contextDir := setupTestContext(t)

				writeContainerfile(contextDir, `
FROM scratch AS stage1

LABEL stage1.label=label-from-stage1

FROM stage1

LABEL final.stage.label=label-from-final-stage
`)

				outputRef := "localhost/test-injecting-buildinfo-unsupported-with-target:" + GenerateUniqueTag(t)

				buildParams := BuildParams{
					Context:   contextDir,
					OutputRef: outputRef,
					Push:      false,
					Target:    "stage1",
				}

				container := setupBuildContainerWithCleanup(t, buildParams, nil)

				err := runBuild(container, buildParams)
				Expect(err).ToNot(HaveOccurred())

				exists := fileExistsInOutputImage(container, outputRef, "/usr/share/buildinfo/labels.json")
				Expect(exists).To(BeFalse(), "Should not have injected /usr/share/buildinfo/labels.json")

				exists2 := fileExistsInOutputImage(container, outputRef, "/root/buildinfo/labels.json")
				Expect(exists2).To(BeFalse(), "Should not have injected /root/buildinfo/labels.json")
			})
		})
	})

	t.Run("DisinheritLabels", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)

		writeContainerfile(contextDir, fmt.Sprintf(`
# Real base image used for an earlier stage
FROM %s AS stage1

LABEL stage1.label=label-from-stage1

FROM stage1

LABEL final.stage.label=value-gets-overridden
LABEL final.stage.label=label-from-final-stage
`, baseImage))

		outputRef := "localhost/test-disinherit-labels:" + GenerateUniqueTag(t)

		buildParams := BuildParams{
			Context:         contextDir,
			OutputRef:       outputRef,
			Push:            false,
			SourceDateEpoch: "1767225600", // 2026-01-01
			InheritLabels:   boolptr(false),
			Labels:          []string{"cli.label=label-from-CLI"},
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil)

		err := runBuild(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		imageMeta := getImageMeta(container, outputRef)
		Expect(formatAsKeyValuePairs(imageMeta.labels)).To(ConsistOf(
			"final.stage.label=label-from-final-stage",
			"cli.label=label-from-CLI",
			"org.opencontainers.image.created=2026-01-01T00:00:00Z",
			// And nothing else (hence ConsistOf())
		))

		injectedLabels := getLabelsFromLabelsJson(container, outputRef)
		Expect(formatAsKeyValuePairs(injectedLabels)).To(ConsistOf(
			"final.stage.label=label-from-final-stage",
			"cli.label=label-from-CLI",
			"org.opencontainers.image.created=2026-01-01T00:00:00Z",
			// And nothing else (hence ConsistOf())
		))
	})

	t.Run("DontSkipUnusedStages", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)
		writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s
LABEL stage=stage0
RUN echo stage 0 was built

FROM scratch AS target
LABEL stage=target

FROM image.does.not/exist:1 AS stage-after-target
`, baseImage))

		outputRef := "localhost/test-dont-skip-unused-stages:" + GenerateUniqueTag(t)

		buildParams := BuildParams{
			Context:          contextDir,
			OutputRef:        outputRef,
			Push:             false,
			Target:           "target",
			SkipUnusedStages: boolptr(false),
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil)

		_, stderr, err := runBuildWithOutput(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		// Stage 0 should be built despite not being needed
		Expect(stderr).To(ContainSubstring("stage 0 was built"))

		// But the target stage should still be "target"
		imageMeta := getImageMeta(container, outputRef)
		Expect(imageMeta.labels).To(HaveKeyWithValue("stage", "target"))
	})

	t.Run("Hermetic", func(t *testing.T) {
		SetupGomega(t)

		t.Run("BlocksAddInstructions", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)
			writeContainerfile(contextDir, `
FROM scratch

# Use an IP directly to prove network is unreachable and the failure isn't just broken DNS.
# (As can happen with e.g. 'BUILDAH_ISOLATION=chroot buildah build --network=none'.)
ADD https://1.1.1.1 /cloudflare-1111.html
`)

			runTest := func(t *testing.T, user string) {
				SetupGomega(t)

				outputRef := "localhost/test-hermetic-blocks-add:" + GenerateUniqueTag(t)

				buildParams := BuildParams{
					Context:   contextDir,
					OutputRef: outputRef,
					Push:      false,
					Hermetic:  true,
					// Disable retries for ADD instructions to make the build fail faster
					ExtraArgs: []string{"--retry=0"},
				}

				var opts []ContainerOption
				if user == "root" {
					opts = append(opts, WithUser("root"), maybeMountContainerStorage(rootStoragePath, "root"))
				}

				container := setupBuildContainerWithCleanup(t, buildParams, nil, opts...)

				_, stderr, err := runBuildWithOutput(container, buildParams)
				Expect(err).To(HaveOccurred())

				// kbc prints the error to stderr
				Expect(stderr).To(ContainSubstring("dial tcp 1.1.1.1:443: connect: network is unreachable"))
			}

			t.Run("AsNonRoot", func(t *testing.T) {
				runTest(t, "taskuser")
			})

			t.Run("AsRoot", func(t *testing.T) {
				runTest(t, "root")
			})
		})

		t.Run("BlocksRunInstructions", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)
			writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s

# Try to connect to port 53 on the Google DNS server.
# Use an IP directly to prove network is unreachable and the failure isn't just broken DNS.
# (As can happen with e.g. 'BUILDAH_ISOLATION=chroot buildah build --network=none'.)
RUN if echo > /dev/tcp/8.8.8.8/53; then echo "Has network access!"; exit 1; fi
`, baseImage))

			runTest := func(t *testing.T, user string) {
				SetupGomega(t)

				outputRef := "localhost/test-hermetic-blocks-curl:" + GenerateUniqueTag(t)

				buildParams := BuildParams{
					Context:   contextDir,
					OutputRef: outputRef,
					Push:      false,
					Hermetic:  true,
				}

				var opts []ContainerOption
				if user == "root" {
					opts = append(opts, WithUser("root"), maybeMountContainerStorage(rootStoragePath, "root"))
				}

				container := setupBuildContainerWithCleanup(t, buildParams, nil, opts...)

				_, stderr, err := runBuildWithOutput(container, buildParams)
				Expect(err).ToNot(HaveOccurred())

				// kbc prints the build logs to stderr
				Expect(stderr).To(ContainSubstring("/dev/tcp/8.8.8.8/53: Network is unreachable"))
			}

			t.Run("AsNonRoot", func(t *testing.T) {
				runTest(t, "taskuser")
			})

			t.Run("AsRoot", func(t *testing.T) {
				runTest(t, "root")
			})
		})

		t.Run("DoesntBlockLoopback", func(t *testing.T) {
			contextDir := setupTestContext(t)
			writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s

# Try to connect to the UDP port 9 (the discard port).
# UDP avoids the "Connection refused" that we would get with TCP because nothing is listening.
RUN echo > /dev/udp/127.0.0.1/9
`, baseImage))

			runTest := func(t *testing.T, user string) {
				SetupGomega(t)

				outputRef := "localhost/test-hermetic-loopback:" + GenerateUniqueTag(t)

				buildParams := BuildParams{
					Context:   contextDir,
					OutputRef: outputRef,
					Push:      false,
					Hermetic:  true,
				}

				var opts []ContainerOption
				if user == "root" {
					opts = append(opts, WithUser("root"), maybeMountContainerStorage(rootStoragePath, "root"))
				}

				container := setupBuildContainerWithCleanup(t, buildParams, nil, opts...)

				_, _, err := runBuildWithOutput(container, buildParams)
				Expect(err).ToNot(HaveOccurred())
			}

			t.Run("AsNonRoot", func(t *testing.T) {
				runTest(t, "taskuser")
			})

			t.Run("AsRoot", func(t *testing.T) {
				runTest(t, "root")
			})
		})

		t.Run("PrePullImages", func(t *testing.T) {
			SetupGomega(t)

			imageRegistry := setupImageRegistry(t)

			createBaseImage := func(name string, randomDataSize int64, base string) string {
				imageRef := imageRegistry.GetTestNamespace() + name + ":" + "test"

				err := CreateTestImage(TestImageConfig{
					ImageRef:       imageRef,
					BaseImage:      base,
					RandomDataSize: randomDataSize,
				})
				Expect(err).ToNot(HaveOccurred())
				// Delete the local image right after this function exits,
				// we want to ensure the build will fail if kbc doesn't pre-pull it
				defer DeleteLocalImage(imageRef)

				_, err = PushImage(imageRef)
				Expect(err).ToNot(HaveOccurred())

				return imageRef
			}

			// Generate new base images for these tests
			// Adding the random data ensures these are unique => not already present in local storage
			baseImage1 := createBaseImage("base1", 1024, "scratch")
			baseImage2 := createBaseImage("base2", 2048, "scratch")
			baseImage3 := createBaseImage("base3", 4096, "scratch")
			realBaseImage := createBaseImage("realbase", 8192, baseImage)
			unusedBaseImage := createBaseImage("unused", 16384, baseImage)

			r := strings.NewReplacer(
				"{baseImage1}", baseImage1,
				"{baseImage2}", baseImage2,
				"{baseImage3}", baseImage3,
				"{realBaseImage}", realBaseImage,
				"{unusedBaseImage}", unusedBaseImage,
			)

			contextDir := setupTestContext(t)
			writeContainerfile(contextDir, r.Replace(`
# This base image is "unused" because the second base1 overrides this stage.
# However, buildah will build *both* of the base1 stages, so we have to pre-pull both of the images.
FROM {unusedBaseImage} AS base1
LABEL base1.real_index=0
RUN echo "the unused stage WAS built"

FROM {baseImage1} AS base1
LABEL base1.real_index=1
# COPY directly from an image
COPY --from={baseImage2} /random-data.bin /data/baseImage2.bin

FROM {realBaseImage} AS realbase1
# COPY from a previous stage
COPY --from=base1 /random-data.bin     /data/baseImage1.bin
COPY --from=base1 /data/baseImage2.bin /data/baseImage2.bin
# Mount directly from an image
RUN --mount=from={baseImage3},src=/random-data.bin,dst=/tmp/baseImage3.bin \
	cp /tmp/baseImage3.bin /data/baseImage3.bin

FROM {realBaseImage} AS realbase2
# Mount from a previous stage
RUN --mount=from=realbase1,src=/data,dst=/tmp/data \
	cp -r /tmp/data /data

# Unused stage, skipped by both buildah and our pre-pull logic
FROM image.does.not/exist:1 AS unused-stage-1
COPY --from=image.does.not/exist:2 /foo /bar

# FROM a previous stage
FROM realbase2
RUN cp /random-data.bin /data/realBaseImage.bin
`))

			outputRef := "localhost/test-hermetic-pre-pull-images:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:   contextDir,
				OutputRef: outputRef,
				Push:      false,
				Hermetic:  true,
			}

			container := setupBuildContainerWithCleanup(t, buildParams, imageRegistry)

			_, stderr, err := runBuildWithOutput(container, buildParams)
			// Main check: no error (would fail without pre-pulling)
			Expect(err).ToNot(HaveOccurred())

			// Verify that buildah really did build the unused stage (otherwise we wasted a pull)
			Expect(stderr).To(ContainSubstring("the unused stage WAS built"))

			// Verify that the correct base was pulled for each FROM/from
			// by checking the sizes of the random-data files
			stdout2 := runWithMountedOutputImage(container, outputRef, "cd $CONTAINER_ROOT; du -b data/*")
			Expect(stdout2).To(SatisfyAll(
				ContainSubstring("1024\tdata/baseImage1.bin"),
				ContainSubstring("2048\tdata/baseImage2.bin"),
				ContainSubstring("4096\tdata/baseImage3.bin"),
				ContainSubstring("8192\tdata/realBaseImage.bin"),
			))

			// Re-run the same scenario with --target=base1
			t.Run("WithTarget", func(t *testing.T) {
				SetupGomega(t)

				outputRef := "localhost/test-hermetic-pre-pull-with-target:" + GenerateUniqueTag(t)
				buildParams.OutputRef = outputRef
				buildParams.Target = "base1"

				// Need clean storage to test the pre-pulling
				newStorage, err := createContainerStorageDir()
				t.Cleanup(func() { removeContainerStorageDir(newStorage) })
				Expect(err).ToNot(HaveOccurred())

				container := setupBuildContainerWithCleanup(t, buildParams, imageRegistry,
					maybeMountContainerStorage(newStorage, "taskuser"))

				_, stderr, err = runBuildWithOutput(container, buildParams)
				Expect(err).ToNot(HaveOccurred())

				labels := getImageMeta(container, outputRef).labels
				Expect(labels).To(HaveKeyWithValue("base1.real_index", "1"),
					"--target=base1 should have matched the second base1, not the first")

				stdout, _, err := container.ExecuteCommandWithOutput(
					"buildah", "images", "--format", "{{.Name}}:{{.Tag}}",
				)
				Expect(err).ToNot(HaveOccurred())
				lines := strings.Split(strings.TrimSpace(stdout), "\n")
				// Should have pulled the images for both the base1 stage, because
				// buildah builds both of them (https://github.com/containers/buildah/issues/6731).
				Expect(lines).To(ConsistOf(
					outputRef,
					// base image for first base1
					unusedBaseImage,
					// base image for second base1
					baseImage1,
					// COPY --from image in second base1
					baseImage2,
				))

				// Verify that buildah really did build the unused stage (otherwise we wasted a pull)
				Expect(stderr).To(ContainSubstring("the unused stage WAS built"))
			})
		})
	})

	t.Run("ImagePullProxy", func(t *testing.T) {
		SetupGomega(t)

		proxyAddr, connectedHosts := startForwardProxy(t)

		contextDir := setupTestContext(t)
		writeContainerfile(contextDir, fmt.Sprintf("FROM %s\n", baseImage))

		// Use a clean container storage dir for this test to ensure the image gets pulled
		containerStorage, err := createContainerStorageDir()
		t.Cleanup(func() { removeContainerStorageDir(containerStorage) })
		Expect(err).ToNot(HaveOccurred())

		outputRef := "localhost/test-image-pull-proxy:" + GenerateUniqueTag(t)

		buildParams := BuildParams{
			Context:        contextDir,
			OutputRef:      outputRef,
			Push:           false,
			ImagePullProxy: "http://" + proxyAddr,
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil,
			maybeMountContainerStorage(containerStorage, "taskuser"))

		err = runBuild(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		_, proxiedRegistry := connectedHosts.Load("registry.access.redhat.com:443")
		Expect(proxiedRegistry).To(BeTrue(),
			"Expected a CONNECT request to registry.access.redhat.com:443 through the proxy")
	})

	t.Run("YumReposD", func(t *testing.T) {
		SetupGomega(t)

		t.Run("MountRepos", func(t *testing.T) {
			SetupGomega(t)

			reposBaseDir := t.TempDir()
			testutil.WriteFileTree(t, reposBaseDir, map[string]string{
				"repos1/a.repo": strings.Repeat("a", 10),
				"repos1/b.repo": strings.Repeat("b", 10),
				// will overwrite repos1/b.repo, verify by checking file size
				"repos2/b.repo": strings.Repeat("b", 1000),
				"repos2/c.repo": strings.Repeat("c", 10),
				// only handles top-level files
				"repos2/subdir/ignored-nested.repo": "ignored",
			})
			// skips symlinks
			err := os.Symlink(
				"../file-outside-mountpoint.txt",
				filepath.Join(reposBaseDir, "repos2", "ignored-symlink.repo"),
			)
			Expect(err).ToNot(HaveOccurred())

			contextDir := setupTestContext(t)
			writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s

RUN cd /etc/yum.repos.d && du -b * > /tmp/repos-during-build.txt

# root should have permissions to write to /etc/yum.repos.d
USER root
RUN echo foo > /etc/yum.repos.d/foo.repo

# for backwards compatibility, non-root should also have these permissions
USER 2000
RUN echo bar > /etc/yum.repos.d/bar.repo
`, baseImage))

			outputRef := "localhost/test-yum-repos-d:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:          contextDir,
				OutputRef:        outputRef,
				Push:             false,
				YumReposDSources: []string{"/repos/repos1", "/repos/repos2"},
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil,
				WithVolumeWithOptions(reposBaseDir, "/repos", "z"))

			err = runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			reposDuringBuild := runWithMountedOutputImage(container, outputRef,
				"cat $CONTAINER_ROOT/tmp/repos-during-build.txt")
			reposInBuiltImage := runWithMountedOutputImage(container, outputRef,
				"cd $CONTAINER_ROOT/etc/yum.repos.d && du -b *")

			Expect(reposDuringBuild).To(SatisfyAll(
				// should have the mounted repos
				MatchRegexp(`10\s+a.repo`),
				MatchRegexp(`1000\s+b.repo`),
				MatchRegexp(`10\s+c.repo`),
				// and not files from subdirectories or symlinks
				Not(ContainSubstring("ignored-nested.repo")),
				Not(ContainSubstring("ignored-symlink.repo")),
				// and not the original repos from the base image
				Not(ContainSubstring("ubi.repo")),
			))
			Expect(reposInBuiltImage).To(SatisfyAll(
				// should have the original repos from the base image
				ContainSubstring("ubi.repo"),
				// and not the mounted repos
				Not(ContainSubstring("a.repo")),
				Not(ContainSubstring("b.repo")),
				Not(ContainSubstring("c.repo")),
				// and not the repos created at build time
				// (they're written to the mount, not the container FS)
				Not(ContainSubstring("foo.repo")),
				Not(ContainSubstring("bar.repo")),
			))
		})

		t.Run("MountEmptyDir", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)
			writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s

RUN ls -l /etc/yum.repos.d > /tmp/repos-during-build.txt
`, baseImage))

			emptyReposDir := t.TempDir()

			outputRef := "localhost/test-yum-repos-d:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:          contextDir,
				OutputRef:        outputRef,
				Push:             false,
				YumReposDSources: []string{"/repos"},
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil,
				WithVolumeWithOptions(emptyReposDir, "/repos", "z"))

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			reposDuringBuild := runWithMountedOutputImage(container, outputRef,
				"cat $CONTAINER_ROOT/tmp/repos-during-build.txt")
			reposInBuiltImage := runWithMountedOutputImage(container, outputRef,
				"cd $CONTAINER_ROOT/etc/yum.repos.d && ls")

			Expect(strings.TrimSpace(reposDuringBuild)).To(Equal("total 0"))
			Expect(reposInBuiltImage).To(ContainSubstring("ubi.repo"))
		})

		t.Run("DontMountAny", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)
			writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s

RUN cd /etc/yum.repos.d && du -b * > /tmp/repos-during-build.txt
`, baseImage))

			outputRef := "localhost/test-yum-repos-d:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:   contextDir,
				OutputRef: outputRef,
				Push:      false,
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil)

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			reposDuringBuild := runWithMountedOutputImage(container, outputRef,
				"cat $CONTAINER_ROOT/tmp/repos-during-build.txt")
			reposInBuiltImage := runWithMountedOutputImage(container, outputRef,
				"cd $CONTAINER_ROOT/etc/yum.repos.d && du -b *")

			Expect(reposDuringBuild).To(ContainSubstring("ubi.repo"))
			Expect(reposDuringBuild).To(Equal(reposInBuiltImage))
		})
	})

	t.Run("PrefetchIntegration", func(t *testing.T) {
		SetupGomega(t)

		t.Run("InjectEnv", func(t *testing.T) {
			SetupGomega(t)

			prefetchDir := t.TempDir()
			// Needs group-write permissions so that taskuser can write to it
			Expect(os.Chmod(prefetchDir, 0775)).To(Succeed())

			testutil.WriteFileTree(t, prefetchDir, map[string]string{
				"prefetch.env": "export PREFETCH_ENV_VAR=foo",
			})

			contextDir := setupTestContext(t)
			writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s AS base

RUN echo "base: PREFETCH_ENV_VAR=$PREFETCH_ENV_VAR"

# Test that injection works with a heredoc that explicitly includes the interpreter
RUN bash <<EOF
echo "heredoc: PREFETCH_ENV_VAR=$PREFETCH_ENV_VAR"
EOF

FROM base

LABEL testlabel=prefetch-integration

# Test that injection doesn't break --mount flags
RUN --mount=from=base,src=/etc/os-release,dst=/tmp/os-release \
    if [ -e /tmp/os-release ]; then echo "mount worked"; fi && \
    echo "final stage: PREFETCH_ENV_VAR=$PREFETCH_ENV_VAR"
`, baseImage))

			outputRef := "localhost/test-prefetch:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:     contextDir,
				OutputRef:   outputRef,
				Push:        false,
				PrefetchDir: "/prefetch",
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil,
				WithVolumeWithOptions(prefetchDir, "/prefetch", "z"))

			_, stderr, err := runBuildWithOutput(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			Expect(stderr).To(ContainSubstring("mount worked"))
			Expect(stderr).To(ContainSubstring("base: PREFETCH_ENV_VAR=foo"))
			Expect(stderr).To(ContainSubstring("heredoc: PREFETCH_ENV_VAR=foo"))
			Expect(stderr).To(ContainSubstring("final stage: PREFETCH_ENV_VAR=foo"))

			// Test that labels.json injection works when prefetch is enabled
			// (both code paths modify the Containerfile)
			injectedLabels := getLabelsFromLabelsJson(container, outputRef)
			Expect(injectedLabels).To(HaveKeyWithValue("testlabel", "prefetch-integration"))
		})

		t.Run("InjectWithTrickyComments", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)
			writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s AS base

RUN echo && \
    # Did you know you can just put a comment in the middle of a RUN instruction?
    echo "RUN 1: PREFETCH_ENV_VAR=${PREFETCH_ENV_VAR-unset}"

RUN --mount=from=%s,src=/etc/os-release,dst=/tmp/os-release \
    # Also between flags, of course.
    --network=host \
    echo "RUN 2: PREFETCH_ENV_VAR=${PREFETCH_ENV_VAR-unset}"

RUN \
    # And the comment can end with a line continuation, which is ignored anyway \
    echo "RUN 3: PREFETCH_ENV_VAR=${PREFETCH_ENV_VAR-unset}"

RUN # But not if there's another token in front of the comment. \
    In that case, the line continuation is honored and this whole \
    RUN instruction is a comment (which is a no-op, not an error).

RUN --network=host # This is also a no-op.

RUN echo "RUN 6: PREFETCH_ENV_VAR=${PREFETCH_ENV_VAR-unset}" # And this works as expected.
`, baseImage, baseImage))

			outputRef := "localhost/test-prefetch:" + GenerateUniqueTag(t)

			// Build without prefetch first to verify injection doesn't alter the expected behavior
			buildParams := BuildParams{
				Context:   contextDir,
				OutputRef: outputRef,
				Push:      false,
			}
			container := setupBuildContainerWithCleanup(t, buildParams, nil)
			_, stderr, err := runBuildWithOutput(container, buildParams)
			Expect(err).ToNot(HaveOccurred())
			Expect(stderr).To(ContainSubstring("RUN 1: PREFETCH_ENV_VAR=unset"))
			Expect(stderr).To(ContainSubstring("RUN 2: PREFETCH_ENV_VAR=unset"))
			Expect(stderr).To(ContainSubstring("RUN 3: PREFETCH_ENV_VAR=unset"))
			Expect(stderr).To(ContainSubstring("RUN 6: PREFETCH_ENV_VAR=unset"))

			// Then build with prefetch, verify it still works and the var is set as expected
			prefetchDir := t.TempDir()
			Expect(os.Chmod(prefetchDir, 0775)).To(Succeed())
			testutil.WriteFileTree(t, prefetchDir, map[string]string{
				"prefetch.env": "export PREFETCH_ENV_VAR=foo",
			})
			buildParams.PrefetchDir = "/prefetch"
			container = setupBuildContainerWithCleanup(t, buildParams, nil,
				WithVolumeWithOptions(prefetchDir, "/prefetch", "z"))
			_, stderr, err = runBuildWithOutput(container, buildParams)
			Expect(err).ToNot(HaveOccurred())
			Expect(stderr).To(ContainSubstring("RUN 1: PREFETCH_ENV_VAR=foo"))
			Expect(stderr).To(ContainSubstring("RUN 2: PREFETCH_ENV_VAR=foo"))
			Expect(stderr).To(ContainSubstring("RUN 3: PREFETCH_ENV_VAR=foo"))
			Expect(stderr).To(ContainSubstring("RUN 6: PREFETCH_ENV_VAR=foo"))
		})

		t.Run("InjectSkipUnsupported", func(t *testing.T) {
			SetupGomega(t)

			prefetchDir := t.TempDir()
			// Needs group-write permissions so that taskuser can write to it
			Expect(os.Chmod(prefetchDir, 0775)).To(Succeed())

			testutil.WriteFileTree(t, prefetchDir, map[string]string{
				"prefetch.env": "export PREFETCH_ENV_VAR=foo",
			})

			contextDir := setupTestContext(t)
			writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s

# heredoc without interpreter
RUN <<EOF
echo "heredoc: PREFETCH_ENV_VAR=${PREFETCH_ENV_VAR-unset}"
EOF

# exec form
RUN ["/bin/sh", "-c", "echo \"exec: PREFETCH_ENV_VAR=${PREFETCH_ENV_VAR-unset}\""]
`, baseImage))

			outputRef := "localhost/test-prefetch:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:     contextDir,
				OutputRef:   outputRef,
				Push:        false,
				PrefetchDir: "/prefetch",
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil,
				WithVolumeWithOptions(prefetchDir, "/prefetch", "z"))

			_, stderr, err := runBuildWithOutput(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			Expect(stderr).To(ContainSubstring("heredoc: PREFETCH_ENV_VAR=unset"))
			Expect(stderr).To(ContainSubstring("exec: PREFETCH_ENV_VAR=unset"))

			Expect(stderr).To(ContainSubstring("skipping unsupported RUN instruction on line 5 (heredoc)"))
			Expect(stderr).To(ContainSubstring("skipping unsupported RUN instruction on line 10 (exec form)"))
		})

		t.Run("SecretMountSupportsAllRunForms", func(t *testing.T) {
			SetupGomega(t)

			prefetchDir := t.TempDir()
			Expect(os.Chmod(prefetchDir, 0775)).To(Succeed())

			testutil.WriteFileTree(t, prefetchDir, map[string]string{
				"prefetch.env":      "export PREFETCH_ENV_VAR=foo",
				"prefetch-env.json": `[{"name":"PREFETCH_ENV_VAR","value":"foo"}]`,
			})

			contextDir := setupTestContext(t)
			writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s

# shell form
RUN echo "shell: PREFETCH_ENV_VAR=${PREFETCH_ENV_VAR-unset}"

# heredoc without interpreter - unsupported by Containerfile editing, supported by secret mounts
RUN <<EOF
echo "heredoc: PREFETCH_ENV_VAR=${PREFETCH_ENV_VAR-unset}"
EOF

# exec form - unsupported by Containerfile editing, supported by secret mounts
RUN ["/bin/sh", "-c", "echo \"exec: PREFETCH_ENV_VAR=${PREFETCH_ENV_VAR-unset}\""]
`, baseImage))

			outputRef := "localhost/test-prefetch:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:     contextDir,
				OutputRef:   outputRef,
				Push:        false,
				PrefetchDir: "/prefetch",
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil,
				WithVolumeWithOptions(prefetchDir, "/prefetch", "z"))

			_, stderr, err := runBuildWithOutput(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			Expect(stderr).To(ContainSubstring("Using buildah secret env mounts"))

			Expect(stderr).To(ContainSubstring("shell: PREFETCH_ENV_VAR=foo"))
			Expect(stderr).To(ContainSubstring("heredoc: PREFETCH_ENV_VAR=foo"))
			Expect(stderr).To(ContainSubstring("exec: PREFETCH_ENV_VAR=foo"))

			Expect(stderr).ToNot(ContainSubstring("skipping unsupported"))

			// Containerfile should not have been modified (no copy created)
			Expect(stderr).ToNot(ContainSubstring("Modifying containerfile"))
		})

		t.Run("InjectDoesntMangleHeredocs", func(t *testing.T) {
			SetupGomega(t)

			prefetchDir := t.TempDir()
			// Needs group-write permissions so that taskuser can write to it
			Expect(os.Chmod(prefetchDir, 0775)).To(Succeed())

			testutil.WriteFileTree(t, prefetchDir, map[string]string{
				"prefetch.env": "export PREFETCH_ENV_VAR=foo",
			})

			contextDir := setupTestContext(t)
			writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s

COPY <<example.Containerfile \
     <<"example2.Containerfile" /tmp
# Lines that look like RUN inside a COPY heredoc should not be modified
RUN echo hi
example.Containerfile
# Same goes for the second heredoc in the same instruction of course
RUN echo $FOO
example2.Containerfile

# The actual RUN instruction *should* be modified to set the env var
RUN sh <<'EOF'
function RUN() {
    echo "Run: $*"
}
# A line that looks like RUN inside a RUN heredoc should not be modified
# => in the output, we should see "Run: echo PREFETCH_ENV_VAR=foo"
#                             not "Run: . /tmp/prefetch.env"
RUN echo "PREFETCH_ENV_VAR=${PREFETCH_ENV_VAR-unset}"
EOF
`, baseImage))

			outputRef := "localhost/test-prefetch:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:     contextDir,
				OutputRef:   outputRef,
				Push:        false,
				PrefetchDir: "/prefetch",
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil,
				WithVolumeWithOptions(prefetchDir, "/prefetch", "z"))

			_, stderr, err := runBuildWithOutput(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			// RUN instruction was handled as expected
			Expect(stderr).To(ContainSubstring("Run: echo PREFETCH_ENV_VAR=foo"))
			Expect(stderr).ToNot(ContainSubstring("Run: . /tmp/prefetch.env"))

			// COPY heredocs were left untouched as expected
			file1 := getFileContentFromOutputImage(container, outputRef, "/tmp/example.Containerfile")
			lines := slices.Collect(strings.Lines(file1))
			Expect(lines).To(Equal([]string{
				"# Lines that look like RUN inside a COPY heredoc should not be modified\n",
				"RUN echo hi\n",
			}))

			file2 := getFileContentFromOutputImage(container, outputRef, "/tmp/example2.Containerfile")
			lines2 := slices.Collect(strings.Lines(file2))
			Expect(lines2).To(Equal([]string{
				"# Same goes for the second heredoc in the same instruction of course\n",
				"RUN echo $FOO\n",
			}))
		})

		t.Run("MountDeps", func(t *testing.T) {
			SetupGomega(t)

			prefetchDir := t.TempDir()
			// Needs group-write permissions so that taskuser can write to it
			Expect(os.Chmod(prefetchDir, 0775)).To(Succeed())

			testutil.WriteFileTree(t, prefetchDir, map[string]string{
				"output/deps/gomod/foo.txt":                    "",
				"output/deps/pip/bar.txt":                      "",
				"output/deps/rpm/x86_64/repos.d/hermeto.repo":  "",
				"output/deps/rpm/aarch64/repos.d/hermeto.repo": "",
				"output/deps/rpm/s390x/repos.d/hermeto.repo":   "",
				"output/deps/rpm/ppc64le/repos.d/hermeto.repo": "",
			})

			contextDir := setupTestContext(t)
			writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s

RUN cd /tmp/.prefetch-output/deps      && echo * > /tmp/deps-dirs.txt
RUN cd /tmp/.prefetch-output/deps/rpm/ && echo * > /tmp/rpm-dirs.txt
RUN cd /etc/yum.repos.d         && echo * > /tmp/yum-repos.txt

USER 2000
# Any user should be able to modify prefetch resources during the build
RUN echo foo > /tmp/.prefetch-output/deps/gomod/user-created.txt
`, baseImage))

			outputRef := "localhost/test-prefetch:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:     contextDir,
				OutputRef:   outputRef,
				Push:        false,
				PrefetchDir: "/prefetch",
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil,
				WithVolumeWithOptions(prefetchDir, "/prefetch", "z"))

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			depsDirs := getFileContentFromOutputImage(container, outputRef, "/tmp/deps-dirs.txt")
			rpmDirs := getFileContentFromOutputImage(container, outputRef, "/tmp/rpm-dirs.txt")
			yumRepos := getFileContentFromOutputImage(container, outputRef, "/tmp/yum-repos.txt")

			Expect(strings.TrimSpace(depsDirs)).To(Equal("gomod pip rpm"))
			// should mount only the matching RPM arch depending on developer's machine
			Expect(strings.TrimSpace(rpmDirs)).To(Or(Equal("x86_64"), Equal("aarch64")))
			Expect(strings.TrimSpace(yumRepos)).To(Equal("hermeto.repo"))

			userCreatedFile := filepath.Join(prefetchDir, "output/deps/gomod/user-created.txt")
			Expect(userCreatedFile).To(Not(BeAnExistingFile()),
				"Modifications during build should not modify original prefetch dir")

			Expect(fileExistsInOutputImage(container, outputRef, "/tmp/.prefetch-output")).To(BeFalse(),
				"Mount point should not persist in built image even if written to")

			// By default, prefetch resources are copied to prefetchDir/copy-*
			entries, err := os.ReadDir(prefetchDir)
			Expect(err).ToNot(HaveOccurred())
			Expect(entries).To(HaveLen(1),
				"Copy directory created during build should not persist after build")
		})

		t.Run("CompatPaths", func(t *testing.T) {
			SetupGomega(t)

			// Does not need group-write permissions here, we copy to a different location
			prefetchDir := t.TempDir()
			testutil.WriteFileTree(t, prefetchDir, map[string]string{
				// Test that cachi2.env works as well
				"cachi2.env":                  "export PREFETCH_ENV_VAR=foo\n",
				"output/deps/generic/foo.txt": "",
			})

			contextDir := setupTestContext(t)
			writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s

RUN echo "PREFETCH_ENV_VAR=$PREFETCH_ENV_VAR"
# Test that we're able to recreate the same structure used current in Konflux
RUN cat /cachi2/cachi2.env
RUN ls /cachi2/output/deps/generic
`, baseImage))

			outputRef := "localhost/test-prefetch:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:             contextDir,
				OutputRef:           outputRef,
				Push:                false,
				PrefetchDir:         "/prefetch",
				PrefetchDirCopy:     "/workspace/prefetch-copy",
				PrefetchEnvMount:    "/cachi2/cachi2.env",
				PrefetchOutputMount: "/cachi2/output",
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil,
				WithVolumeWithOptions(prefetchDir, "/prefetch", "z"))

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			Expect(filepath.Join(contextDir, "prefetch-copy")).ToNot(BeAnExistingFile(),
				"Copy directory created during build should not persist after build")
		})
	})

	t.Run("YumReposAndPrefetchRepo", func(t *testing.T) {
		SetupGomega(t)

		reposDir := t.TempDir()
		testutil.WriteFileTree(t, reposDir, map[string]string{
			"my.repo": "[my-repo]",
			// prefetch repos take priority, this gets overwritten
			"hermeto.repo": "[not-real-hermeto-repo]",
		})

		prefetchDir := t.TempDir()
		// Needs group-write permissions so that taskuser can write to it
		Expect(os.Chmod(prefetchDir, 0775)).To(Succeed())
		testutil.WriteFileTree(t, prefetchDir, map[string]string{
			"output/deps/rpm/x86_64/repos.d/hermeto.repo":  "[hermeto-repo]",
			"output/deps/rpm/aarch64/repos.d/hermeto.repo": "[hermeto-repo]",
		})

		contextDir := setupTestContext(t)
		writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s

RUN echo "my.repo=$(cat /etc/yum.repos.d/my.repo)"
RUN echo "hermeto.repo=$(cat /etc/yum.repos.d/hermeto.repo)"
`, baseImage))

		outputRef := "localhost/test-prefetch:" + GenerateUniqueTag(t)

		buildParams := BuildParams{
			Context:          contextDir,
			OutputRef:        outputRef,
			Push:             false,
			PrefetchDir:      "/prefetch",
			YumReposDSources: []string{"/repos"},
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil,
			WithVolumeWithOptions(prefetchDir, "/prefetch", "z"),
			WithVolumeWithOptions(reposDir, "/repos", "z"))

		_, stderr, err := runBuildWithOutput(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		Expect(stderr).To(ContainSubstring("my.repo=[my-repo]"))
		Expect(stderr).To(ContainSubstring("hermeto.repo=[hermeto-repo]"))
	})

	t.Run("ResolvedBaseImages", func(t *testing.T) {
		SetupGomega(t)

		// The canonical references that should be on the right side in resolved-base-images.txt
		// (if the input reference has a tag, it should resolve to canonicalWithTag, otherwise canonicalNoTag)
		canonicalWithTag := "registry.access.redhat.com/ubi10/ubi-micro:10.1-1766049088@sha256:2946fa1b951addbcd548ef59193dc0af9b3e9fedb0287b4ddb6e697b06581622"
		canonicalNoTag := "registry.access.redhat.com/ubi10/ubi-micro@sha256:2946fa1b951addbcd548ef59193dc0af9b3e9fedb0287b4ddb6e697b06581622"

		// The non-canonical references. Note that we can't really use one without a tag,
		// because then the digest would not be predictable. The digest is only predictable because we use
		// UBI {version}-{release} tags, which are treated as immutable.
		fullyQualifiedWithTag := "registry.access.redhat.com/ubi10/ubi-micro:10.1-1766049088"
		shortWithTag := "ubi10/ubi-micro:10.1-1766049088"
		shortWithDigest := "ubi10/ubi-micro@sha256:2946fa1b951addbcd548ef59193dc0af9b3e9fedb0287b4ddb6e697b06581622"
		shortWithDigestAndTag := "ubi10/ubi-micro:10.1-1766049088@sha256:2946fa1b951addbcd548ef59193dc0af9b3e9fedb0287b4ddb6e697b06581622"

		replacer := strings.NewReplacer(
			"{canonicalWithTag}", canonicalWithTag,
			"{canonicalNoTag}", canonicalNoTag,
			"{fullyQualifiedWithTag}", fullyQualifiedWithTag,
			"{shortWithTag}", shortWithTag,
			"{shortWithDigest}", shortWithDigest,
			"{shortWithDigestAndTag}", shortWithDigestAndTag,
		)

		contextDir := setupTestContext(t)
		writeContainerfile(contextDir, replacer.Replace(`
FROM {canonicalWithTag} AS stage1

COPY --from={canonicalNoTag} /etc/os-release /tmp/os-release

RUN --mount=from={fullyQualifiedWithTag},src=/etc/os-release,dst=/tmp/os-release true

FROM {shortWithDigestAndTag}

COPY --from=stage1 /tmp/os-release /tmp/os-release

COPY --from={shortWithDigest} /etc/os-release /tmp/os-release

COPY --from={shortWithTag} /etc/os-release /tmp/os-release
`))

		outputRef := "localhost/resolved-base-images-output:" + GenerateUniqueTag(t)

		buildParams := BuildParams{
			Context:                  contextDir,
			OutputRef:                outputRef,
			Push:                     false,
			ResolvedBaseImagesOutput: "/workspace/resolved-base-images.txt",
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil,
			WithEnv("CONTAINERS_REGISTRIES_CONF", "/tmp/registries.conf"))

		// make it possible to test the canonicalization of image refs,
		// e.g. if the containerfile has just "ubi10/ubi-micro" we want to make it fully qualified
		container.CreateFileInContainer(
			"/tmp/registries.conf", `unqualified-search-registries = ["registry.access.redhat.com"]`)

		err := runBuild(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		resolvedBaseImages, err := os.ReadFile(filepath.Join(contextDir, "resolved-base-images.txt"))
		Expect(err).ToNot(HaveOccurred())

		lines := strings.Split(strings.TrimSuffix(string(resolvedBaseImages), "\n"), "\n")
		Expect(lines).To(ConsistOf(
			fmt.Sprintf("%s %s", canonicalNoTag, canonicalNoTag),
			fmt.Sprintf("%s %s", canonicalWithTag, canonicalWithTag),
			fmt.Sprintf("%s %s", fullyQualifiedWithTag, canonicalWithTag),
			fmt.Sprintf("%s %s", shortWithTag, canonicalWithTag),
			fmt.Sprintf("%s %s", shortWithDigestAndTag, canonicalWithTag),
			fmt.Sprintf("%s %s", shortWithDigest, canonicalNoTag),
		))
	})

	t.Run("ResolvedBaseImagesSkipsUnpullable", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)

		writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s AS builder

FROM oci:./base_image

COPY --from=builder /etc/os-release /tmp/os-release
`, baseImage))

		outputRef := "localhost/resolved-base-images-unpullable:" + GenerateUniqueTag(t)

		buildParams := BuildParams{
			Context:                  contextDir,
			OutputRef:                outputRef,
			Push:                     false,
			ResolvedBaseImagesOutput: "/workspace/resolved-base-images.txt",
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil)

		err := container.ExecuteCommand("buildah", "pull", baseImage)
		Expect(err).ToNot(HaveOccurred())
		err = container.ExecuteCommand(
			"buildah", "push", "--remove-signatures", baseImage, "oci:/workspace/base_image",
		)
		Expect(err).ToNot(HaveOccurred())

		err = runBuild(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		resolvedBaseImages, err := os.ReadFile(filepath.Join(contextDir, "resolved-base-images.txt"))
		Expect(err).ToNot(HaveOccurred())

		content := string(resolvedBaseImages)
		Expect(content).ToNot(ContainSubstring("oci:"))

		lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
		Expect(lines).To(ConsistOf(
			fmt.Sprintf("%s %s", baseImage, baseImage),
		))
	})

	t.Run("RHSM", func(t *testing.T) {
		SetupGomega(t)

		t.Run("DisablesHostIntegration", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)

			writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s

RUN bash -e <<EOF
if [[ -d /run/secrets/etc-pki-entitlement ]]; then
    echo "FAIL: entitlements from host mounted!"
    exit 1
else
    echo "OK: entitlements from host NOT mounted"
fi

if [[ -d /run/secrets/rhsm ]]; then
    echo "FAIL: rhsm from host mounted!"
    exit 1
else
    echo "OK: rhsm from host NOT mounted"
fi
EOF
`, baseImage))

			outputRef := "localhost/rhsm-disables-host-integration:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:   contextDir,
				OutputRef: outputRef,
				Push:      false,
				// Note: KBC always disables RHSM host integration, even if no RHSM params used
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil,
				// need root to create the files below
				WithUser("root"), maybeMountContainerStorage(rootStoragePath, "root"))

			// Files that need to exist in the container to simulate a host with enabled RHSM integration
			filesToCreate := map[string]string{
				// /etc/containers/mounts.conf takes priority over /usr/share/containers/mounts.conf,
				// use that one to ensure our config is respected
				"/etc/containers/mounts.conf":                                   "/usr/share/rhel/secrets:/run/secrets",
				"/usr/share/rhel/secrets/etc-pki-entitlement/some-cert.pem":     "some entitlement certificate",
				"/usr/share/rhel/secrets/etc-pki-entitlement/some-cert-key.pem": "some entitlement key",
				"/usr/share/rhel/secrets/rhsm/ca/redhat-uep.pem":                "the CA cert for RHSM",
			}
			for path, content := range filesToCreate {
				Expect(container.ExecuteCommand("mkdir", "-p", filepath.Dir(path))).To(Succeed())
				Expect(container.CreateFileInContainer(path, content)).To(Succeed())
			}

			_, stderr, err := runBuildWithOutput(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			Expect(stderr).To(ContainSubstring("OK: entitlements from host NOT mounted"))
			Expect(stderr).To(ContainSubstring("OK: rhsm from host NOT mounted"))

			// Verify that disabling RHSM host integration didn't modify the container FS
			stdout, _, err := container.ExecuteCommandWithOutput("ls", "/usr/share/rhel/secrets")
			Expect(err).ToNot(HaveOccurred())
			lines := strings.Split(strings.TrimSpace(stdout), "\n")
			Expect(lines).To(ContainElements("etc-pki-entitlement", "rhsm"))
		})

		t.Run("Entitlements", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)

			writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s

RUN echo "cert.pem=$(cat /etc/pki/entitlement/cert.pem)" && \
    echo "cert-key.pem=$(cat /etc/pki/entitlement/cert-key.pem)" && \
    echo "ca-from-host.pem=$(cat /etc/rhsm/ca/ca-from-host.pem)"

# Modify the certs to test that this doesn't modify the original host files
RUN echo modified > /etc/pki/entitlement/cert.pem && \
    echo modified > /etc/pki/entitlement/cert-key.pem && \
    echo modified > /etc/rhsm/ca/ca-from-host.pem
`, baseImage))

			outputRef := "localhost/rhsm-entitlements:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:          contextDir,
				OutputRef:        outputRef,
				Push:             false,
				RHSMEntitlements: "/tmp/entitlements",
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil)

			filesToCreate := map[string]string{
				"/tmp/entitlements/cert.pem":     "entitlement-cert",
				"/tmp/entitlements/cert-key.pem": "entitlement-key",
				"/etc/rhsm/ca/ca-from-host.pem":  "CA from host",
			}
			for path, content := range filesToCreate {
				Expect(container.ExecuteCommand("mkdir", "-p", filepath.Dir(path))).To(Succeed())
				Expect(container.CreateFileInContainer(path, content)).To(Succeed())
			}

			_, stderr, err := runBuildWithOutput(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			Expect(stderr).To(ContainSubstring("cert.pem=entitlement-cert"))
			Expect(stderr).To(ContainSubstring("cert-key.pem=entitlement-key"))
			Expect(stderr).To(ContainSubstring("ca-from-host.pem=CA from host"))

			// Verify the build wasn't able to modify the original host files
			Expect(container.GetFileContent("/tmp/entitlements/cert.pem")).To(Equal("entitlement-cert"))
			Expect(container.GetFileContent("/tmp/entitlements/cert-key.pem")).To(Equal("entitlement-key"))
			Expect(container.GetFileContent("/etc/rhsm/ca/ca-from-host.pem")).To(Equal("CA from host"))
		})

		t.Run("ActivationKeyMount", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)

			writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s

RUN echo "activationkey=$(cat /activation-key/activationkey)" && \
    echo "org=$(cat /activation-key/org)"

RUN bash -e <<EOF
if [[ -e /etc/rhsm/ca/ca-from-host.pem ]]; then
    echo "the build does self-registration, ca-from-host.pem should not have been mounted!"
    exit 1
fi
EOF

# 'subscription-manager register' creates files in these directories.
# Create them ourselves to test that they don't persist in the built image.
RUN mkdir -p /etc/pki/entitlement && echo new-entitlement-cert > /etc/pki/entitlement/cert.pem && \
    mkdir -p /etc/pki/consumer    && echo new-consumer-cert    > /etc/pki/consumer/cert.pem

# Modify the secrets to test that this doesn't modify the original host files
RUN echo modified > /activation-key/activationkey && \
    echo modified > /activation-key/org
`, baseImage))

			outputRef := "localhost/rhsm-activation-key:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:             contextDir,
				OutputRef:           outputRef,
				Push:                false,
				RHSMActivationKey:   "/tmp/activation-key.txt",
				RHSMOrg:             "/tmp/org.txt",
				RHSMActivationMount: "/activation-key",
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil)

			filesToCreate := map[string]string{
				"/tmp/activation-key.txt": "my-activation-key",
				"/tmp/org.txt":            "my-org",
				// create the cert to verify kbc *does not* mount it
				"/etc/rhsm/ca/ca-from-host.pem": "CA from host",
			}
			for path, content := range filesToCreate {
				Expect(container.CreateFileInContainer(path, content)).To(Succeed())
			}

			_, stderr, err := runBuildWithOutput(container, buildParams)
			Expect(err).ToNot(HaveOccurred())

			Expect(stderr).To(ContainSubstring("activationkey=my-activation-key"))
			Expect(stderr).To(ContainSubstring("org=my-org"))

			// Verify the build wasn't able to modify the original host files
			Expect(container.GetFileContent("/tmp/activation-key.txt")).To(Equal("my-activation-key"))
			Expect(container.GetFileContent("/tmp/org.txt")).To(Equal("my-org"))

			// Verify that the certs created during the build don't persist in the built image
			entitlementExists := fileExistsInOutputImage(container, outputRef, "/etc/pki/entitlement/cert.pem")
			Expect(entitlementExists).To(BeFalse(), "/etc/pki/entitlement/cert.pem should not be in the built image!")
			consumerExists := fileExistsInOutputImage(container, outputRef, "/etc/pki/consumer/cert.pem")
			Expect(consumerExists).To(BeFalse(), "/etc/pki/consumer/cert.pem should not be in the built image!")
		})

		// Pre-registration isn't testable without either a working activation key
		// (and a dependency on the actual RHSM servers) or a local RHSM deployment
		// (which is a nightmare). Pre-registration has unit test coverage instead.
		// t.Run("ActivationKeyPreregistration", func(t *testing.T) {})
	})

	t.Run("Squash", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)
		writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s
RUN echo one > /file1.txt
RUN echo two > /file2.txt
`, baseImage))

		outputRef := "localhost/test-squash:" + GenerateUniqueTag(t)

		buildParams := BuildParams{
			Context:   contextDir,
			OutputRef: outputRef,
			Squash:    true,
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil)

		err := runBuild(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		imageMeta := getImageMeta(container, outputRef)
		Expect(imageMeta.layer_ids).To(HaveLen(1),
			"--squash should collapse all layers into one")
	})

	t.Run("OmitHistory", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)
		writeContainerfile(contextDir, `
FROM scratch
LABEL test.label="omit-history-test"
`)

		outputRef := "localhost/test-omit-history:" + GenerateUniqueTag(t)

		buildParams := BuildParams{
			Context:     contextDir,
			OutputRef:   outputRef,
			OmitHistory: true,
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil)

		err := runBuild(container, buildParams)
		Expect(err).ToNot(HaveOccurred())

		imageMeta := getImageMeta(container, outputRef)
		Expect(imageMeta.history).To(BeEmpty(),
			"--omit-history should produce no history entries")
	})

	t.Run("Capabilities", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)
		writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s

RUN bash -e <<'EOF'
while read -r key value; do
    if [[ $key == 'CapEff:' ]]; then
        echo "CapEff: $value"
    fi
done < /proc/self/status
EOF
`, baseImage))

		outputRef := "localhost/test-devices:" + GenerateUniqueTag(t)

		buildParams := BuildParams{
			Context:   contextDir,
			OutputRef: outputRef,
			CapAdd:    []string{"CHOWN,DAC_OVERRIDE", "DAC_READ_SEARCH"},
			CapDrop:   []string{"ALL"},
		}

		container := setupBuildContainerWithCleanup(t, buildParams, nil)

		_, stderr, err := runBuildWithOutput(container, buildParams)
		Expect(err).ToNot(HaveOccurred())
		// Should have dropped everything but the first 3 caps (0...00111 binary)
		Expect(stderr).To(ContainSubstring("CapEff: 0000000000000007"))
	})

	t.Run("WrongArchBase", func(t *testing.T) {
		SetupGomega(t)

		foreignArch := "linux/s390x"
		if runtime.GOARCH == "s390x" {
			foreignArch = "linux/amd64"
		}

		imageRegistry := setupImageRegistry(t)
		baseImageRepo := imageRegistry.GetTestNamespace() + "wrong-arch-base"
		tag := GenerateUniqueTag(t)
		foreignArchImageRef := baseImageRepo + ":" + tag

		err := CreateTestImage(TestImageConfig{
			ImageRef: foreignArchImageRef,
			Platform: foreignArch,
		})
		Expect(err).ToNot(HaveOccurred())
		t.Cleanup(func() { DeleteLocalImage(foreignArchImageRef) })

		foreignArchDigest, err := PushImage(foreignArchImageRef)
		Expect(err).ToNot(HaveOccurred())

		t.Run("SingleArchDigest", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)
			imageRefByDigest := baseImageRepo + "@" + foreignArchDigest
			writeContainerfile(contextDir, fmt.Sprintf("FROM %s\n", imageRefByDigest))

			outputRef := "localhost/wrong-arch-digest:" + GenerateUniqueTag(t)
			buildParams := BuildParams{
				Context:   contextDir,
				OutputRef: outputRef,
			}

			container := setupBuildContainerWithCleanup(t, buildParams, imageRegistry)
			_, stderr, err := runBuildWithOutput(container, buildParams)
			Expect(err).To(HaveOccurred())
			Expect(stderr).To(ContainSubstring("has architecture"))
			Expect(stderr).To(ContainSubstring("Use a multi-arch image reference instead of a single-architecture reference"))
		})

		t.Run("SingleArchDigestWithPlatform", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)
			imageRefByDigest := baseImageRepo + "@" + foreignArchDigest
			writeContainerfile(contextDir, fmt.Sprintf("FROM --platform=%s %s\n", foreignArch, imageRefByDigest))

			outputRef := "localhost/wrong-arch-platform:" + GenerateUniqueTag(t)
			buildParams := BuildParams{
				Context:   contextDir,
				OutputRef: outputRef,
			}

			container := setupBuildContainerWithCleanup(t, buildParams, imageRegistry)
			_, stderr, err := runBuildWithOutput(container, buildParams)
			Expect(err).To(HaveOccurred())
			Expect(stderr).To(ContainSubstring("has architecture"))
			Expect(stderr).To(ContainSubstring("Use a multi-arch image reference instead of a single-architecture reference"))
		})

		t.Run("SingleArchDigestWithAllowFlag", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)
			imageRefByDigest := baseImageRepo + "@" + foreignArchDigest
			writeContainerfile(contextDir, fmt.Sprintf("FROM %s\n", imageRefByDigest))

			outputRef := "localhost/wrong-arch-allow:" + GenerateUniqueTag(t)
			buildParams := BuildParams{
				Context:                  contextDir,
				OutputRef:                outputRef,
				AllowCrossPlatformImages: true,
			}

			container := setupBuildContainerWithCleanup(t, buildParams, imageRegistry)
			_, stderr, err := runBuildWithOutput(container, buildParams)
			Expect(err).ToNot(HaveOccurred())
			Expect(stderr).To(ContainSubstring("Cross-platform copy is a risky operation"))
		})

		t.Run("PlatformWithBuildplatformArg", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)
			writeContainerfile(contextDir, fmt.Sprintf("ARG BUILDPLATFORM\nFROM --platform=$BUILDPLATFORM %s\n", baseImage))

			outputRef := "localhost/buildplatform-arg:" + GenerateUniqueTag(t)
			buildParams := BuildParams{
				Context:   contextDir,
				OutputRef: outputRef,
			}

			container := setupBuildContainerWithCleanup(t, buildParams, imageRegistry)
			_, stderr, err := runBuildWithOutput(container, buildParams)
			Expect(err).ToNot(HaveOccurred())
			Expect(stderr).ToNot(ContainSubstring("Cross-platform copy is a risky operation"))
		})

		t.Run("PlatformWithCustomArg", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)
			imageRefByDigest := baseImageRepo + "@" + foreignArchDigest
			writeContainerfile(contextDir, fmt.Sprintf("ARG MYPLATFORM=%s\nFROM --platform=$MYPLATFORM %s\n", foreignArch, imageRefByDigest))

			outputRef := "localhost/wrong-arch-customarg:" + GenerateUniqueTag(t)
			buildParams := BuildParams{
				Context:                  contextDir,
				OutputRef:                outputRef,
				AllowCrossPlatformImages: true,
			}

			container := setupBuildContainerWithCleanup(t, buildParams, imageRegistry)
			_, stderr, err := runBuildWithOutput(container, buildParams)
			Expect(err).ToNot(HaveOccurred())
			Expect(stderr).To(ContainSubstring("Cross-platform copy is a risky operation"))
		})

		t.Run("HermeticCrossPlatformPrePull", func(t *testing.T) {
			SetupGomega(t)

			contextDir := setupTestContext(t)
			imageRefByDigest := baseImageRepo + "@" + foreignArchDigest
			writeContainerfile(contextDir, fmt.Sprintf("FROM %s\n", imageRefByDigest))

			outputRef := "localhost/wrong-arch-hermetic-prepull:" + GenerateUniqueTag(t)
			buildParams := BuildParams{
				Context:                  contextDir,
				OutputRef:                outputRef,
				Hermetic:                 true,
				AllowCrossPlatformImages: true,
			}

			newStorage, err := createContainerStorageDir()
			t.Cleanup(func() { removeContainerStorageDir(newStorage) })
			Expect(err).ToNot(HaveOccurred())

			container := setupBuildContainerWithCleanup(t, buildParams, imageRegistry,
				maybeMountContainerStorage(newStorage, "taskuser"))
			_, stderr, err := runBuildWithOutput(container, buildParams)
			Expect(err).ToNot(HaveOccurred())
			Expect(stderr).To(ContainSubstring("Cross-platform copy is a risky operation"))
		})

		t.Run("IndexWithoutHostArch", func(t *testing.T) {
			SetupGomega(t)

			indexTag := GenerateUniqueTag(t)
			indexRef := baseImageRepo + "-index:" + indexTag
			_, err := CreateAndPushImageIndex(indexRef, []string{foreignArchImageRef})
			Expect(err).ToNot(HaveOccurred())

			contextDir := setupTestContext(t)
			writeContainerfile(contextDir, fmt.Sprintf("FROM %s\n", indexRef))

			outputRef := "localhost/wrong-arch-index:" + GenerateUniqueTag(t)
			buildParams := BuildParams{
				Context:   contextDir,
				OutputRef: outputRef,
			}

			container := setupBuildContainerWithCleanup(t, buildParams, imageRegistry)
			_, stderr, err := runBuildWithOutput(container, buildParams)
			Expect(err).To(HaveOccurred())
			Expect(stderr).To(ContainSubstring("no image found in image index for architecture"))
		})
	})

	// Test that we're able to pull images with opaque whiteouts;
	// motivated by https://github.com/podman-container-tools/buildah/issues/6903
	t.Run("PullImageWithOpaqueWhiteout", func(t *testing.T) {
		SetupGomega(t)

		imageRegistry := setupImageRegistry(t)

		// Build a base image with an opaque whiteout and push it to the registry
		setupBaseImage := func() string {
			contextDir := setupTestContext(t)
			writeContainerfile(contextDir, fmt.Sprintf(`
FROM %s
# Removing and re-creating directory in single instruction should create an opaque whiteout
RUN rm -r /etc/yum.repos.d && mkdir /etc/yum.repos.d
`, baseImage))

			outputRef := imageRegistry.GetTestNamespace() + "opaque-whiteout" + ":" + "base"

			container := setupBuildContainerWithCleanup(t, BuildParams{Context: contextDir}, imageRegistry)

			Expect(container.ExecuteCommand("mkdir", "-p", "/home/taskuser/.config/containers")).To(Succeed())
			// To get an opaque whiteout, we need buildah to leave layer diff creation to the kernel
			// (i.e. we need a setup where 'buildah info' reports "Native Overlay Diff": "true").
			// The default task-runner config doesn't do that, override with one that might.
			Expect(container.CreateFileInContainer(
				"/home/taskuser/.config/containers/storage.conf", "[storage]\ndriver = \"overlay\"",
			)).To(Succeed())

			Expect(container.ExecuteCommand("buildah", "build", "-t", outputRef, "/workspace")).To(Succeed())
			Expect(container.ExecuteCommand("buildah", "push", outputRef)).To(Succeed())

			// Push the base image to the context dir so that we can verify we have an opaque whiteout
			err = container.ExecuteCommand(
				"buildah", "push", "--remove-signatures", outputRef, "oci:/workspace/baseimage",
			)
			Expect(err).ToNot(HaveOccurred())

			blobs, err := filepath.Glob(filepath.Join(contextDir, "baseimage", "blobs", "sha256", "*"))
			Expect(err).ToNot(HaveOccurred())

			var whiteouts []string
			for _, blobPath := range blobs {
				layerFiles, isLayer := listLayerFiles(blobPath)
				if !isLayer {
					continue
				}
				for _, file := range layerFiles {
					if strings.HasPrefix(filepath.Base(file), ".wh.") {
						whiteouts = append(whiteouts, file)
					}
				}
			}

			Expect(whiteouts).To(ContainElement("etc/yum.repos.d/.wh..wh..opq"),
				"Test failed to create an opaque whiteout in the base image layers")

			// Delete the image from local storage to force the subsequent tests to pull it
			Expect(container.ExecuteCommand("buildah", "rmi", outputRef)).To(Succeed())

			return outputRef
		}

		baseImageWithWhiteouts := setupBaseImage()

		contextDir := setupContext(t)
		writeContainerfile(contextDir, "FROM "+baseImageWithWhiteouts)

		runTest := func(t *testing.T, user string) {
			SetupGomega(t)

			outputRef := "localhost/test-pull-image-opaque-whiteout:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:   contextDir,
				OutputRef: outputRef,
				Push:      false,
			}

			var opts []ContainerOption
			if user == "root" {
				opts = append(opts, WithUser("root"), maybeMountContainerStorage(rootStoragePath, "root"))
			}

			container := setupBuildContainerWithCleanup(t, buildParams, imageRegistry, opts...)

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())
		}

		// https://github.com/podman-container-tools/buildah/issues/6903 affects only root
		t.Run("AsRoot", func(t *testing.T) {
			runTest(t, "root")
		})

		// ...but test non-root as well to make sure (since we already know the behavior is different)
		t.Run("AsNonRoot", func(t *testing.T) {
			runTest(t, "taskuser")
		})
	})

	t.Run("SyftSBOM", func(t *testing.T) {
		SetupGomega(t)

		contextDir := setupTestContext(t)
		testutil.WriteFileTree(t, contextDir, map[string]string{
			"requirements.txt": "some-dependency==1.0.0",
		})

		writeContainerfile(contextDir, fmt.Sprintf(`FROM %s`, baseImage))

		runTest := func(t *testing.T, user string, sbomFormat string) {
			SetupGomega(t)

			outputRef := "localhost/test-syft-sbom:" + GenerateUniqueTag(t)

			buildParams := BuildParams{
				Context:          contextDir,
				OutputRef:        outputRef,
				SyftSourceOutput: "/workspace/sbom-source.json",
				SyftImageOutput:  "/workspace/sbom-image.json",
				SBOMFormat:       sbomFormat,
			}

			var opts []ContainerOption
			if user == "root" {
				opts = append(opts, WithUser("root"), maybeMountContainerStorage(rootStoragePath, "root"))
			}

			container := setupBuildContainerWithCleanup(t, buildParams, nil, opts...)

			err := runBuild(container, buildParams)
			Expect(err).ToNot(HaveOccurred())
		}

		t.Run("SPDX", func(t *testing.T) {
			runTest(t, "root", "spdx")

			sbomSource := readSPDX(filepath.Join(contextDir, "sbom-source.json"))
			Expect(sbomSource.packageNames()).To(ContainElement("some-dependency"))

			sbomImage := readSPDX(filepath.Join(contextDir, "sbom-image.json"))
			Expect(sbomImage.packageNames()).To(ContainElements("glibc", "gpg-pubkey", "redhat-release"))
		})

		t.Run("CycloneDX", func(t *testing.T) {
			runTest(t, "root", "cyclonedx")

			sbomSource := readCycloneDX(filepath.Join(contextDir, "sbom-source.json"))
			Expect(sbomSource.componentNames()).To(ContainElement("some-dependency"))

			sbomImage := readCycloneDX(filepath.Join(contextDir, "sbom-image.json"))
			Expect(sbomImage.componentNames()).To(ContainElements("glibc", "gpg-pubkey", "redhat-release"))
		})

		// Test that we can successfully scan the built image (involves 'buildah mount') as non-root
		t.Run("AsNonRoot", func(t *testing.T) {
			runTest(t, "taskuser", "spdx")

			sbomSource := readSPDX(filepath.Join(contextDir, "sbom-source.json"))
			Expect(sbomSource.packageNames()).To(ContainElement("some-dependency"))

			sbomImage := readSPDX(filepath.Join(contextDir, "sbom-image.json"))
			Expect(sbomImage.packageNames()).To(ContainElements("glibc", "gpg-pubkey", "redhat-release"))
		})
	})
}
