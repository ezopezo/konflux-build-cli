package integration_tests_framework

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/onsi/gomega"

	cliWrappers "github.com/konflux-ci/konflux-build-cli/pkg/cliwrappers"
	l "github.com/konflux-ci/konflux-build-cli/pkg/logger"
)

const (
	// Name of the CLI binary
	KonfluxBuildCli = "konflux-build-cli"
	// Keep task-runner image reference in one place, so it's consistent across tests.
	TaskRunnerImageRef = "quay.io/konflux-ci/task-runner:1.8.1@sha256:b9ef0479b57a494368a12a02886c650314c9ac7391c6228f21065865aab71c9e"
)

var (
	containerTool string
	cliBinPath    string
)

func init() {
	compileDir := os.TempDir()
	cliBinPath = filepath.Join(compileDir, KonfluxBuildCli)

	// Init logger
	logLevel := "info"
	logLevelEnv := os.Getenv("KBC_LOG_LEVEL")
	if logLevelEnv != "" {
		logLevel = logLevelEnv
	}
	if err := l.InitLogger(logLevel); err != nil {
		fmt.Printf("failed to init logger: %s", err.Error())
		os.Exit(2)
	}

	// Detect container tool to use
	if ct := os.Getenv("KBC_TEST_CONTAINER_TOOL"); ct != "" {
		containerTool = ct
	} else if podmanInstalled, _ := cliWrappers.CheckCliToolAvailable("podman"); podmanInstalled {
		containerTool = "podman"
	} else if dockerInstalled, _ := cliWrappers.CheckCliToolAvailable("docker"); dockerInstalled {
		containerTool = "docker"
	} else {
		l.Logger.Fatal("no container engine found")
	}

	// Compile the CLI only once for all tests
	if err := CompileKonfluxCli(); err != nil {
		l.Logger.Fatal(err)
	}
}

func NewImageRegistry() ImageRegistry {
	if LocalRegistry {
		return NewZotRegistry()
	}
	return NewQuayRegistry()
}

func GetCliBinPath() string {
	return cliBinPath
}

func IsKonfluxCliCompiled() bool {
	return FileExists(cliBinPath)
}

func FindRepoRoot() string {
	// Handle running from root and test folder
	var mainGoPath = "main.go"
	if !FileExists(mainGoPath) {
		mainGoPath = path.Join("..", mainGoPath)
	}
	return path.Dir(mainGoPath)
}

func CompileKonfluxCli() error {
	executor := cliWrappers.NewCliExecutor()

	var mainGoPath = path.Join(FindRepoRoot(), "main.go")

	os.Setenv("CGO_ENABLED", "0")
	os.Setenv("GOOS", "linux")
	compileArgs := []string{"build"}
	if Debug {
		compileArgs = append(compileArgs, "-gcflags", "all=-N -l")
	}
	compileArgs = append(compileArgs, "-o", cliBinPath, mainGoPath)
	stdout, stderr, _, err := executor.Execute(cliWrappers.Command("go", compileArgs...))
	if err != nil {
		fmt.Printf("failed to build CLI: %s\n[stdout]:\n%s\n[stderr]:\n%s\n", err.Error(), stdout, stderr)
	}
	return err
}

func FileExists(filepath string) bool {
	stat, err := os.Stat(filepath)
	if os.IsNotExist(err) {
		return false
	}
	return !stat.IsDir()
}

func EnsureDirectory(dirPath string) error {
	_, err := os.Stat(dirPath)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(dirPath, 0755); err != nil {
			return err
		}
	}
	return nil
}

// CreateTempDir creates a directory in OS temp dir with given prefix
// and returns full path to the creted directory.
func CreateTempDir(prefix string) (string, error) {
	tmpDir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", err
	}
	err = os.Chmod(tmpDir, 0777)
	if err != nil {
		return "", err
	}
	return tmpDir, nil
}

func SaveToTempFile(data []byte) (string, error) {
	tmpFile, err := os.CreateTemp("", "tmp-*")
	if err != nil {
		return "", err
	}
	if _, err := tmpFile.Write(data); err != nil {
		os.Remove(tmpFile.Name())
		return "", err
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpFile.Name())
		return "", err
	}
	return tmpFile.Name(), nil
}

func CreateFileWithRandomContent(fileName string, size int64) error {
	file, err := os.Create(fileName)
	if err != nil {
		return err
	}
	defer file.Close()

	if _, err = io.CopyN(file, rand.Reader, size); err != nil {
		return err
	}
	return nil
}

type TestImageConfig struct {
	// Image to create and push
	ImageRef string
	// Image platform, e.g. linux/amd64 or linux/arm64
	Platform string
	// Image to base onto.
	// If empty string, scratch is used.
	BaseImage string
	// Files to add into the image: path in container -> path on host
	Files map[string]string
	// Labels to add to the image
	Labels map[string]string
	// Add a ramdom data file of given size.
	// Skip generation if the value is not positive.
	RandomDataSize int64
	// Format of the built image's manifest and metadata (oci or docker)
	// If empty, defaults to oci
	BuildahFormat string
}

func CreateTestImage(config TestImageConfig) error {
	const dataFileName = "random-data.bin"

	executor := cliWrappers.NewCliExecutor()

	testImageDir, err := CreateTempDir("test-image-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(testImageDir)

	baseImage := config.BaseImage
	if baseImage == "" {
		baseImage = "scratch"
	}

	dockerfileContent := []string{}
	dockerfileContent = append(dockerfileContent, "FROM "+baseImage)
	for labelName, labelValue := range config.Labels {
		dockerfileContent = append(dockerfileContent, fmt.Sprintf(`LABEL %s="%s"`, labelName, labelValue))
	}

	for filePathInContainer, filePathOnHost := range config.Files {
		fileNameInContextDir := filepath.Base(filePathOnHost)
		dockerfileContent = append(dockerfileContent, fmt.Sprintf("COPY %s %s", fileNameInContextDir, filePathInContainer))
		stdout, stderr, _, err := executor.Execute(cliWrappers.Command("cp", filePathOnHost, path.Join(testImageDir, fileNameInContextDir)))
		if err != nil {
			fmt.Printf("failed to copy test file: %s\n[stdout]:\n%s\n[stderr]:\n%s\n", err.Error(), stdout, stderr)
			return err
		}
	}

	if config.RandomDataSize > 0 {
		dockerfileContent = append(dockerfileContent, fmt.Sprintf("COPY %s %s", dataFileName, dataFileName))
		if err := CreateFileWithRandomContent(path.Join(testImageDir, dataFileName), config.RandomDataSize); err != nil {
			return err
		}
	}

	dockerfileContentString := strings.Join(dockerfileContent, "\n")
	if err := os.WriteFile(path.Join(testImageDir, "Dockerfile"), []byte(dockerfileContentString), 0644); err != nil {
		return err
	}

	buildArgs := []string{"build", "--tag", config.ImageRef}
	if config.Platform != "" {
		buildArgs = append(buildArgs, "--platform", config.Platform)
	}
	// --format is only supported by buildah/podman, not docker
	if config.BuildahFormat != "" {
		if containerTool == "docker" {
			return fmt.Errorf("BuildahFormat is not supported with docker, only buildah/podman support --format flag")
		}
		buildArgs = append(buildArgs, "--format", config.BuildahFormat)
	}
	buildArgs = append(buildArgs, ".")
	stdout, stderr, _, err := executor.Execute(cliWrappers.Cmd{Name: containerTool, Args: buildArgs, Dir: testImageDir})
	if err != nil {
		fmt.Printf("failed to build test image: %s\n[stdout]:\n%s\n[stderr]:\n%s\n", err.Error(), stdout, stderr)
		return err
	}

	return nil
}

// CreateAndPushImageIndex pushes image index that includes given images.
// Returns digest of the pushed image index.
func CreateAndPushImageIndex(indexRef string, images []string) (string, error) {
	if indexRef == "" {
		return "", errors.New("index reference should not be empty")
	}
	if len(images) < 1 {
		return "", errors.New("index should contain at least one image")
	}
	if slices.Contains(images, "") {
		return "", errors.New("image reference should not be empty")
	}

	executor := cliWrappers.NewCliExecutor()

	// Create image index
	createManifestArgs := []string{"manifest", "create"}
	// Add --amend to prevent manifest already exist failure on test re-run.
	createManifestArgs = append(createManifestArgs, "--amend")
	createManifestArgs = append(createManifestArgs, indexRef)
	createManifestArgs = append(createManifestArgs, images...)
	if stdout, stderr, _, err := executor.Execute(cliWrappers.Command(containerTool, createManifestArgs...)); err != nil {
		fmt.Printf("failed to create image index: %s\n[stdout]:\n%s\n[stderr]:\n%s\n", err.Error(), stdout, stderr)
		return "", err
	}

	// Clean up local image index
	defer func() {
		if stdout, stderr, _, err := executor.Execute(cliWrappers.Command(containerTool, "manifest", "rm", indexRef)); err != nil {
			fmt.Printf("failed to clean up local image index: %s\n[stdout]:\n%s\n[stderr]:\n%s\n", err.Error(), stdout, stderr)
		}
	}()

	// Push image index
	digest := ""

	switch containerTool {
	case "docker":
		stdout, stderr, _, err := executor.Execute(cliWrappers.Command(containerTool, "manifest", "push", indexRef))
		if err != nil {
			fmt.Printf("failed to push image index: %s\n[stdout]:\n%s\n[stderr]:\n%s\n", err.Error(), stdout, stderr)
			return "", err
		}
		digest = digestRegex.FindString(stdout + "\n" + stderr)

	case "podman":
		const digestfilePath = "/tmp/index-digestfile"
		stdout, stderr, _, err := executor.Execute(cliWrappers.Command("podman", "push", "--digestfile", digestfilePath, indexRef))
		if err != nil {
			fmt.Printf("failed to push image index: %s\n[stdout]:\n%s\n[stderr]:\n%s\n", err.Error(), stdout, stderr)
			return "", err
		}
		defer os.Remove(digestfilePath)

		digestBytes, err := os.ReadFile(digestfilePath)
		if err != nil {
			return "", fmt.Errorf("failed to read digest file: %s", err.Error())
		}
		digest = string(digestBytes)
	}

	return digest, nil
}

func DeleteLocalImage(imageRef string) error {
	executor := cliWrappers.NewCliExecutor()
	stdout, stderr, _, err := executor.Execute(cliWrappers.Command(containerTool, "rmi", imageRef))
	if err != nil {
		fmt.Printf("failed to remove test image: %s\n[stdout]:\n%s\n[stderr]:\n%s\n", err.Error(), stdout, stderr)
	}
	return err
}

var digestRegex = regexp.MustCompile(`sha256:[a-f0-9]{64}`)

// PushImage pushes given image into registry and returns its digest.
func PushImage(imageRef string) (string, error) {
	executor := cliWrappers.NewCliExecutor()

	switch containerTool {
	case "docker":
		stdout, stderr, _, err := executor.Execute(cliWrappers.Command("docker", "push", imageRef))
		if err != nil {
			fmt.Printf("failed to push test image: %s\n[stdout]:\n%s\n[stderr]:\n%s\n", err.Error(), stdout, stderr)
			return "", err
		}
		return digestRegex.FindString(stdout + "\n" + stderr), nil

	case "podman":
		const digestfilePath = "/tmp/digestfile"
		stdout, stderr, _, err := executor.Execute(cliWrappers.Command("podman", "push", "--digestfile", digestfilePath, imageRef))
		if err != nil {
			fmt.Printf("failed to push test image: %s\n[stdout]:\n%s\n[stderr]:\n%s\n", err.Error(), stdout, stderr)
			return "", err
		}
		defer os.Remove(digestfilePath)

		digest, err := os.ReadFile(digestfilePath)
		if err != nil {
			return "", fmt.Errorf("failed to read digest file: %s", err.Error())
		}
		return string(digest), nil
	}
	return "", fmt.Errorf("unknow container tool %s", containerTool)
}

// Generates a unique container image tag using the test name and a random suffix.
func GenerateUniqueTag(t *testing.T) string {
	randomBytes := make([]byte, 4)
	_, err := rand.Read(randomBytes)
	if err != nil {
		t.Fatalf("Failed to generate random tag: %v", err)
	}
	randomSuffix := hex.EncodeToString(randomBytes)
	// Replace slashes with hyphens to make the tag valid for subtests
	testName := strings.ReplaceAll(t.Name(), "/", "-")
	return fmt.Sprintf("%s-%s", testName, randomSuffix)
}

// Registers the Gomega failure handler for the test.
func SetupGomega(t *testing.T) {
	gomega.RegisterFailHandler(func(message string, callerSkip ...int) {
		fmt.Printf("Test Failure: %s\n", message)
		t.FailNow()
	})
}
