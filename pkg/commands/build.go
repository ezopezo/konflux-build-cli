package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/containers/image/v5/docker/reference"
	cliWrappers "github.com/konflux-ci/konflux-build-cli/pkg/cliwrappers"
	"github.com/konflux-ci/konflux-build-cli/pkg/common"
	dfeditor "github.com/konflux-ci/konflux-build-cli/pkg/common/containerfile_editor"
	"github.com/opencontainers/go-digest"
	"github.com/package-url/packageurl-go"
	"github.com/spf13/cobra"

	"github.com/containerd/platforms"
	"github.com/keilerkonzept/dockerfile-json/pkg/buildargs"
	"github.com/keilerkonzept/dockerfile-json/pkg/dockerfile"
	l "github.com/konflux-ci/konflux-build-cli/pkg/logger"
	"github.com/moby/buildkit/frontend/dockerfile/instructions"
)

const (
	defaultPrefetchOutputMount = "/tmp/.prefetch-output"
	defaultPrefetchEnvMount    = "/tmp/.prefetch.env"

	envVarInUserNamespace = "_KBC_IN_USER_NAMESPACE"
)

var BuildParamsConfig = map[string]common.Parameter{
	"containerfile": {
		Name:         "containerfile",
		ShortName:    "f",
		EnvVarName:   "KBC_BUILD_CONTAINERFILE",
		TypeKind:     reflect.String,
		DefaultValue: "",
		Usage:        "Path to Containerfile. Tries with prepended --context first before falling back to the direct path.\nIf not specified, uses Containerfile/Dockerfile from the context directory.",
	},
	"context": {
		Name:         "context",
		ShortName:    "c",
		EnvVarName:   "KBC_BUILD_CONTEXT",
		TypeKind:     reflect.String,
		DefaultValue: ".",
		Usage:        "Build context directory.",
	},
	"source": {
		Name:         "source",
		ShortName:    "s",
		EnvVarName:   "KBC_BUILD_SOURCE",
		TypeKind:     reflect.String,
		DefaultValue: "",
		Usage:        "Path to a directory containing the source code.\nIf specified, the --containerfile and --context are treated as (and verified to be) relative to the source.",
	},
	"output-ref": {
		Name:       "output-ref",
		ShortName:  "t",
		EnvVarName: "KBC_BUILD_OUTPUT_REF",
		TypeKind:   reflect.String,
		Usage:      `The reference of the output image - [registry/namespace/]name[:tag]. Required.`,
		Required:   true,
	},
	"push": {
		Name:         "push",
		ShortName:    "",
		EnvVarName:   "KBC_BUILD_PUSH",
		TypeKind:     reflect.Bool,
		DefaultValue: "false",
		Usage:        "Push the built image to the registry.",
	},
	"secret-dirs": {
		Name:       "secret-dirs",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_SECRET_DIRS",
		TypeKind:   reflect.Slice,
		Usage:      "Directories containing secret files to make available during build.",
	},
	"workdir-mount": {
		Name:         "workdir-mount",
		ShortName:    "",
		EnvVarName:   "KBC_BUILD_WORKDIR_MOUNT",
		TypeKind:     reflect.String,
		DefaultValue: "",
		Usage:        "Mount the context directory (which is also the workdir) into the build with '--volume $PWD:$WORKDIR_MOUNT'.",
	},
	"build-args": {
		Name:       "build-args",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_BUILD_ARGS",
		TypeKind:   reflect.Slice,
		Usage:      "Arguments to pass to the build using buildah's --build-arg option.",
	},
	"build-args-file": {
		Name:       "build-args-file",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_BUILD_ARGS_FILE",
		TypeKind:   reflect.String,
		Usage:      "Path to a file with build arguments, see https://www.mankier.com/1/buildah-build#--build-arg-file",
	},
	"envs": {
		Name:       "envs",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_ENVS",
		TypeKind:   reflect.Slice,
		Usage:      "Environment variables to pass to the build using buildah's --env option.",
	},
	"labels": {
		Name:       "labels",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_LABELS",
		TypeKind:   reflect.Slice,
		Usage:      "Labels to apply to the image using buildah's --label option.",
	},
	"annotations": {
		Name:       "annotations",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_ANNOTATIONS",
		TypeKind:   reflect.Slice,
		Usage:      "Annotations to apply to the image using buildah's --annotation option.",
	},
	"annotations-file": {
		Name:       "annotations-file",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_ANNOTATIONS_FILE",
		TypeKind:   reflect.String,
		Usage:      "Path to a file with annotations, same file format as --build-args-file.",
	},
	"image-source": {
		Name:       "image-source",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_IMAGE_SOURCE",
		TypeKind:   reflect.String,
		Usage:      "Set the org.opencontainers.image.source annotation (and label) to this value.",
	},
	"image-revision": {
		Name:       "image-revision",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_IMAGE_REVISION",
		TypeKind:   reflect.String,
		Usage:      "Set the org.opencontainers.image.revision annotation (and label) to this value.",
	},
	"legacy-build-timestamp": {
		Name:       "legacy-build-timestamp",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_LEGACY_BUILD_TIMESTAMP",
		TypeKind:   reflect.String,
		Usage:      "Timestamp for the org.opencontainers.image.created annotation (and label). If not provided, uses the current time.\nThis does NOT behave like buildah's --timestamp option, it only sets the annotation and label.\nConflicts with --source-date-epoch.",
	},
	"source-date-epoch": {
		Name:      "source-date-epoch",
		ShortName: "",
		// Note: intentionally omits the KBC_BUILD_ prefix. SOURCE_DATE_EPOCH is a standard variable.
		EnvVarName: "SOURCE_DATE_EPOCH",
		TypeKind:   reflect.String,
		Usage:      "See https://www.mankier.com/1/buildah-build#--source-date-epoch.\nThe timestamp will also be used for the org.opencontainers.image.created annotation and label.\nConflicts with --legacy-build-timestamp.",
	},
	"rewrite-timestamp": {
		Name:       "rewrite-timestamp",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_REWRITE_TIMESTAMP",
		TypeKind:   reflect.Bool,
		Usage:      "See https://www.mankier.com/1/buildah-build#--rewrite-timestamp. Has no effect if --source-date-epoch is not set.",
	},
	"quay-image-expires-after": {
		Name:       "quay-image-expires-after",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_QUAY_IMAGE_EXPIRES_AFTER",
		TypeKind:   reflect.String,
		Usage:      "Time after which the image expires on quay.io (e.g. 1h, 2d, 3w). Adds the quay.expires-after label.",
	},
	"add-legacy-labels": {
		Name:         "add-legacy-labels",
		ShortName:    "",
		EnvVarName:   "KBC_BUILD_ADD_LEGACY_LABELS",
		TypeKind:     reflect.Bool,
		DefaultValue: "false",
		Usage:        "In addition to OCI annotations and labels, also set projectatomic labels (https://github.com/projectatomic/ContainerApplicationGenericLabels).",
	},
	"containerfile-json-output": {
		Name:       "containerfile-json-output",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_CONTAINERFILE_JSON_OUTPUT",
		TypeKind:   reflect.String,
		Usage:      "Write the parsed Containerfile JSON representation to this path.",
	},
	"skip-injections": {
		Name:         "skip-injections",
		ShortName:    "",
		EnvVarName:   "KBC_BUILD_SKIP_INJECTIONS",
		TypeKind:     reflect.Bool,
		DefaultValue: "false",
		Usage:        "Do not inject anything into /usr/share/buildinfo/.",
	},
	"include-legacy-buildinfo-path": {
		Name:         "include-legacy-buildinfo-path",
		ShortName:    "",
		EnvVarName:   "KBC_BUILD_INCLUDE_LEGACY_BUILDINFO_PATH",
		TypeKind:     reflect.Bool,
		DefaultValue: "false",
		Usage:        "When injecting files to /usr/share/buildinfo/, also inject them to /root/buildinfo/.",
	},
	"inherit-labels": {
		Name:         "inherit-labels",
		ShortName:    "",
		EnvVarName:   "KBC_BUILD_INHERIT_LABELS",
		TypeKind:     reflect.Bool,
		DefaultValue: "true",
		Usage:        "Inherit labels from the base image or base stages.",
	},
	"target": {
		Name:       "target",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_TARGET",
		TypeKind:   reflect.String,
		Usage:      "Target stage in the Containerfile to build. By default, the target stage is the last stage.",
	},
	"skip-unused-stages": {
		Name:         "skip-unused-stages",
		ShortName:    "",
		EnvVarName:   "KBC_BUILD_SKIP_UNUSED_STAGES",
		TypeKind:     reflect.Bool,
		DefaultValue: "true",
		Usage:        "Skip stages in multi-stage builds which don't affect the target stage.",
	},
	"hermetic": {
		Name:         "hermetic",
		ShortName:    "",
		EnvVarName:   "KBC_BUILD_HERMETIC",
		TypeKind:     reflect.Bool,
		DefaultValue: "false",
		Usage:        "Prevent network access while building the containerfile.",
	},
	"image-pull-proxy": {
		Name:       "image-pull-proxy",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_IMAGE_PULL_PROXY",
		TypeKind:   reflect.String,
		Usage:      "Set HTTP_PROXY and HTTPS_PROXY for base image pulls.",
	},
	"image-pull-noproxy": {
		Name:       "image-pull-noproxy",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_IMAGE_PULL_NOPROXY",
		TypeKind:   reflect.String,
		Usage:      "Set NO_PROXY for base image pulls.",
	},
	"yum-repos-d-sources": {
		Name:       "yum-repos-d-sources",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_YUM_REPOS_D_SOURCES",
		TypeKind:   reflect.Slice,
		Usage:      "List of yum.repos.d directories to merge together and mount over the yum-repos-d-target dir.",
	},
	"yum-repos-d-target": {
		Name:         "yum-repos-d-target",
		ShortName:    "",
		EnvVarName:   "KBC_BUILD_YUM_REPOS_D_TARGET",
		TypeKind:     reflect.String,
		DefaultValue: "",
		Usage:        "Set an alternative mount destination for the merged yum-repos-d-sources dir (default is /etc/yum.repos.d).",
	},
	"prefetch-dir": {
		Name:       "prefetch-dir",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_PREFETCH_DIR",
		TypeKind:   reflect.String,
		Usage:      "Directory containing the outputs of the prefetch-dependencies subcommand.\nShould have a prefetch.env file in the root and an output/ subdirectory.",
	},
	"prefetch-dir-copy": {
		Name:       "prefetch-dir-copy",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_PREFETCH_DIR_COPY",
		TypeKind:   reflect.String,
		Usage:      "Set an alternative path where to copy the prefetch directory.\nDefaults to a randomly named directory alongside prefetch-dir. Must not already exist. Removed on exit.",
	},
	"prefetch-output-mount": {
		Name:       "prefetch-output-mount",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_PREFETCH_OUTPUT_MOUNT",
		TypeKind:   reflect.String,
		Usage:      "Set an alternative mount destination for the prefetch output (default is " + defaultPrefetchOutputMount + ").",
	},
	"prefetch-env-mount": {
		Name:       "prefetch-env-mount",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_PREFETCH_ENV_MOUNT",
		TypeKind:   reflect.String,
		Usage:      "Set an alternative mount destination for the prefetch env file (default is " + defaultPrefetchEnvMount + ")\nThis path usually doesn't matter, containerfiles never need to access it explicitly.",
	},
	"resolved-base-images-output": {
		Name:       "resolved-base-images-output",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_RESOLVED_BASE_IMAGES_OUTPUT",
		TypeKind:   reflect.String,
		Usage:      "Take the set of images that the containerfile depends on and write them to the file at the specified path.\nEach line in the file is \"<ref-from-containerfile> <canonical-ref>\",\nwhere canonical-ref includes the fully qualified name, digest and optionaly tag (if ref-from-containerfile has a tag).",
	},
	"rhsm-entitlements": {
		Name:       "rhsm-entitlements",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_RHSM_ENTITLEMENTS",
		TypeKind:   reflect.String,
		Usage:      "Directory with RHSM entitlement certificates.\nSee 'Red Hat Subscription Management' in the help text for more details.",
	},
	"rhsm-activation-key": {
		Name:       "rhsm-activation-key",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_RHSM_ACTIVATION_KEY",
		TypeKind:   reflect.String,
		Usage:      "File containing an RHSM activation key.\nSee 'Red Hat Subscription Management' in the help text for more details.",
	},
	"rhsm-org": {
		Name:       "rhsm-org",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_RHSM_ORG",
		TypeKind:   reflect.String,
		Usage:      "File containing an RHSM organization ID.\nSee 'Red Hat Subscription Management' in the help text for more details.",
	},
	"rhsm-activation-mount": {
		Name:       "rhsm-activation-mount",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_RHSM_ACTIVATION_MOUNT",
		TypeKind:   reflect.String,
		Usage:      "Mount destination for the RHSM activation key and org files.\nSee 'Red Hat Subscription Management' in the help text for more details.",
	},
	"rhsm-activation-preregister": {
		Name:       "rhsm-activation-preregister",
		ShortName:  "",
		EnvVarName: "KBC_BUILD_RHSM_ACTIVATION_PREREGISTER",
		TypeKind:   reflect.Bool,
		Usage:      "Pre-register with RHSM using the provided activation key and org ID.\nWARNING: unregisters your host system if already registered. Requires root permissions.\nSee 'Red Hat Subscription Management' in the help text for more details.",
	},
	"rhsm-mount-ca-certs": {
		Name:         "rhsm-mount-ca-certs",
		ShortName:    "",
		EnvVarName:   "KBC_BUILD_RHSM_MOUNT_CA_CERTS",
		DefaultValue: "auto",
		TypeKind:     reflect.String,
		Usage:        "Mount /etc/rhsm/ca from the host machine into the build. Valid values are 'always', 'auto', 'never'.\nSee 'Red Hat Subscription Management' in the help text for more details.",
	},
	"src-tls-verify": {
		Name:         "src-tls-verify",
		EnvVarName:   "KBC_BUILD_SRC_TLS_VERIFY",
		TypeKind:     reflect.Bool,
		DefaultValue: "true",
		Usage:        "Require HTTPS and verify certificates when accessing source registries.",
	},
	"dest-tls-verify": {
		Name:         "dest-tls-verify",
		EnvVarName:   "KBC_BUILD_DEST_TLS_VERIFY",
		TypeKind:     reflect.Bool,
		DefaultValue: "true",
		Usage:        "Require HTTPS and verify certificates when pushing to the destination registry.",
	},
	"squash": {
		Name:       "squash",
		EnvVarName: "KBC_BUILD_SQUASH",
		TypeKind:   reflect.Bool,
		Usage:      "Squash all layers, including those from base image(s), into one single layer.",
	},
	"omit-history": {
		Name:       "omit-history",
		EnvVarName: "KBC_BUILD_OMIT_HISTORY",
		TypeKind:   reflect.Bool,
		Usage:      "Omit build history information in the built image.",
	},
	"no-cache": {
		Name:       "no-cache",
		EnvVarName: "KBC_BUILD_NO_CACHE",
		TypeKind:   reflect.Bool,
		Usage:      "Do not use existing cached images for the container build.",
	},
	"security-opts": {
		Name:       "security-opts",
		EnvVarName: "KBC_BUILD_SECURITY_OPTS",
		TypeKind:   reflect.Slice,
		Usage:      "Security options to pass to buildah's --security-opt.",
	},
	"cap-add": {
		Name:       "cap-add",
		EnvVarName: "KBC_BUILD_CAP_ADD",
		TypeKind:   reflect.Slice,
		Usage:      "Capabilities to add when running the build.",
	},
	"cap-drop": {
		Name:       "cap-drop",
		EnvVarName: "KBC_BUILD_CAP_DROP",
		TypeKind:   reflect.Slice,
		Usage:      "Capabilities to drop when running the build.",
	},
	"devices": {
		Name:       "devices",
		EnvVarName: "KBC_BUILD_DEVICES",
		TypeKind:   reflect.Slice,
		Usage:      "Additional devices to provide during the build.",
	},
	"ulimits": {
		Name:       "ulimits",
		EnvVarName: "KBC_BUILD_ULIMITS",
		TypeKind:   reflect.Slice,
		Usage:      "Resource limits to pass to buildah's --ulimit.",
	},
}

type BuildParams struct {
	Containerfile              string   `paramName:"containerfile"`
	Context                    string   `paramName:"context"`
	Source                     string   `paramName:"source"`
	OutputRef                  string   `paramName:"output-ref"`
	Push                       bool     `paramName:"push"`
	SecretDirs                 []string `paramName:"secret-dirs"`
	WorkdirMount               string   `paramName:"workdir-mount"`
	BuildArgs                  []string `paramName:"build-args"`
	BuildArgsFile              string   `paramName:"build-args-file"`
	Envs                       []string `paramName:"envs"`
	Labels                     []string `paramName:"labels"`
	Annotations                []string `paramName:"annotations"`
	AnnotationsFile            string   `paramName:"annotations-file"`
	ImageSource                string   `paramName:"image-source"`
	ImageRevision              string   `paramName:"image-revision"`
	LegacyBuildTimestamp       string   `paramName:"legacy-build-timestamp"`
	SourceDateEpoch            string   `paramName:"source-date-epoch"`
	RewriteTimestamp           bool     `paramName:"rewrite-timestamp"`
	QuayImageExpiresAfter      string   `paramName:"quay-image-expires-after"`
	AddLegacyLabels            bool     `paramName:"add-legacy-labels"`
	ContainerfileJsonOutput    string   `paramName:"containerfile-json-output"`
	SkipInjections             bool     `paramName:"skip-injections"`
	InheritLabels              bool     `paramName:"inherit-labels"`
	IncludeLegacyBuildinfoPath bool     `paramName:"include-legacy-buildinfo-path"`
	Target                     string   `paramName:"target"`
	SkipUnusedStages           bool     `paramName:"skip-unused-stages"`
	Hermetic                   bool     `paramName:"hermetic"`
	ImagePullProxy             string   `paramName:"image-pull-proxy"`
	ImagePullNoProxy           string   `paramName:"image-pull-noproxy"`
	YumReposDSources           []string `paramName:"yum-repos-d-sources"`
	YumReposDTarget            string   `paramName:"yum-repos-d-target"`
	PrefetchDir                string   `paramName:"prefetch-dir"`
	PrefetchDirCopy            string   `paramName:"prefetch-dir-copy"`
	PrefetchOutputMount        string   `paramName:"prefetch-output-mount"`
	PrefetchEnvMount           string   `paramName:"prefetch-env-mount"`
	ResolvedBaseImagesOutput   string   `paramName:"resolved-base-images-output"`
	RHSMEntitlements           string   `paramName:"rhsm-entitlements"`
	RHSMActivationKey          string   `paramName:"rhsm-activation-key"`
	RHSMOrg                    string   `paramName:"rhsm-org"`
	RHSMActivationMount        string   `paramName:"rhsm-activation-mount"`
	RHSMActivationPreregister  bool     `paramName:"rhsm-activation-preregister"`
	RHSMMountCACerts           string   `paramName:"rhsm-mount-ca-certs"`
	SrcTLSVerify               bool     `paramName:"src-tls-verify"`
	DestTLSVerify              bool     `paramName:"dest-tls-verify"`
	Squash                     bool     `paramName:"squash"`
	OmitHistory                bool     `paramName:"omit-history"`
	NoCache                    bool     `paramName:"no-cache"`
	SecurityOpts               []string `paramName:"security-opts"`
	CapAdd                     []string `paramName:"cap-add"`
	CapDrop                    []string `paramName:"cap-drop"`
	Devices                    []string `paramName:"devices"`
	Ulimits                    []string `paramName:"ulimits"`
	ExtraArgs                  []string // Additional arguments to pass to buildah build
}

type BuildCliWrappers struct {
	BuildahCli          cliWrappers.BuildahCliInterface
	BuildahUnshare      cliWrappers.WrapperCmd
	Unshare             cliWrappers.WrapperCmd
	SelfInUserNamespace cliWrappers.WrapperCmd
	SubscriptionManager cliWrappers.SubscriptionManagerCliInterface
}

type BuildResults struct {
	ImageUrl string `json:"image_url"`
	Digest   string `json:"digest,omitempty"`
}

type Build struct {
	Params        *BuildParams
	CliWrappers   BuildCliWrappers
	Results       BuildResults
	ResultsWriter common.ResultsWriterInterface

	buildahVersion       cliWrappers.BuildahVersionInfo
	parsedBuildahVersion []int

	containerfilePath string

	// pre-computed buildah arguments
	buildahSecrets        []cliWrappers.BuildahSecret
	buildahVolumes        []cliWrappers.BuildahVolume
	mergedLabels          []string
	mergedAnnotations     []string
	buildinfoBuildContext *cliWrappers.BuildahBuildContext

	// temporary workdir and related paths
	tempWorkdir           string
	containerfileCopyPath string

	// temporary files/directories that could not be placed inside the tempWorkdir
	tempFilesOutsideWorkdir []string

	registeredWithRHSM bool
	// these are constants, but they need to be mockable for tests
	hostEntitlements  string
	hostConsumerCerts string
	hostRHSMcaCerts   string
}

func NewBuild(cmd *cobra.Command, extraArgs []string) (*Build, error) {
	build := &Build{
		hostEntitlements:  "/etc/pki/entitlement",
		hostConsumerCerts: "/etc/pki/consumer",
		hostRHSMcaCerts:   "/etc/rhsm/ca",
	}

	params := &BuildParams{}
	if err := common.ParseParameters(cmd, BuildParamsConfig, params); err != nil {
		return nil, err
	}
	// Store any extra arguments passed after -- separator
	params.ExtraArgs = extraArgs
	build.Params = params

	if err := build.initCliWrappers(); err != nil {
		return nil, err
	}

	build.ResultsWriter = common.NewResultsWriter()

	return build, nil
}

func (c *Build) effectiveContextDir() string {
	if c.Params.Source != "" && !filepath.IsAbs(c.Params.Context) {
		return filepath.Join(c.Params.Source, c.Params.Context)
	} else {
		return c.Params.Context
	}
}

func (c *Build) cleanup() {
	if c.tempWorkdir != "" {
		if err := os.RemoveAll(c.tempWorkdir); err != nil {
			l.Logger.Warnf("Failed to clean up temporary workdir %s: %s", c.tempWorkdir, err)
		}
	}
	for _, p := range c.tempFilesOutsideWorkdir {
		if err := os.RemoveAll(p); err != nil {
			l.Logger.Warnf("Failed to clean up temporary path %s: %s", p, err)
		}
	}
	if c.registeredWithRHSM {
		c.CliWrappers.SubscriptionManager.Unregister()
	}
}

func (c *Build) initCliWrappers() error {
	executor := cliWrappers.NewCliExecutor()

	buildahCli, err := cliWrappers.NewBuildahCli(executor)
	if err != nil {
		return err
	}
	c.CliWrappers.BuildahCli = buildahCli

	c.CliWrappers.BuildahUnshare = cliWrappers.NewWrapperCmd("buildah", "unshare")

	c.CliWrappers.Unshare = cliWrappers.NewWrapperCmd("unshare")

	selfPath, err := os.Executable()
	if err != nil {
		return err
	}
	c.CliWrappers.SelfInUserNamespace = cliWrappers.NewWrapperCmd(selfPath, "internal", "in-user-namespace")

	if c.Params.RHSMActivationPreregister {
		subman, err := cliWrappers.NewSubscriptionManagerCli(executor)
		if err != nil {
			return fmt.Errorf("cannot pre-register with RHSM: %w", err)
		}
		c.CliWrappers.SubscriptionManager = subman
	}

	return nil
}

func (c *Build) ensureTempWorkdirExists() error {
	if c.tempWorkdir == "" {
		tempWorkdir, err := os.MkdirTemp("", "kbc-image-build-")
		if err != nil {
			return fmt.Errorf("creating temporary workdir: %w", err)
		}
		c.tempWorkdir = tempWorkdir
	}

	return nil
}

func (c *Build) ensureContainerfileCopied() error {
	if c.containerfileCopyPath != "" {
		return nil
	}
	containerfileCopy, err := c.copyToTempWorkdir(c.containerfilePath)
	if err != nil {
		return fmt.Errorf("creating containerfile copy: %w", err)
	}
	l.Logger.Debugf("Copied containerfile to %s", containerfileCopy)
	c.containerfileCopyPath = containerfileCopy
	return nil
}

func (c *Build) copyToTempWorkdir(filePath string) (copyPath string, err error) {
	if err := c.ensureTempWorkdirExists(); err != nil {
		return "", err
	}

	infile, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer func() {
		if e := infile.Close(); e != nil && err == nil {
			err = e
		}
	}()

	outfile, err := os.CreateTemp(c.tempWorkdir, filepath.Base(filePath)+"-*")
	if err != nil {
		return "", err
	}
	// Failing to close the outfile could mean that it's not fully written,
	// so handle the Close() errors rather than using defer.

	_, err = io.Copy(outfile, infile)
	if err != nil {
		// we already have an error and want to return that one
		_ = outfile.Close()
		return "", err
	}

	err = outfile.Close()
	return outfile.Name(), err
}

// Run re-execs the command inside a user namespace if not already in one,
// then delegates to run() for the actual logic.
func (c *Build) Run() error {
	if os.Getenv(envVarInUserNamespace) == "" {
		err := c.reExecInUserNamespace()
		if err != nil {
			return fmt.Errorf("re-execing self in a user namespace: %w", err)
		}
		// unreachable; if reExecInUserNamespace succeeds it replaces the current process
	}
	return c.run()
}

func (c *Build) run() error {
	common.LogParameters(BuildParamsConfig, c.Params)
	if len(c.Params.ExtraArgs) > 0 {
		l.Logger.Infof("[extra args]: %v", c.Params.ExtraArgs)
	}

	defer c.cleanup()

	if err := c.validateParams(); err != nil {
		return err
	}

	if err := c.detectBuildahVersion(); err != nil {
		return err
	}

	if err := c.detectContainerfile(); err != nil {
		return err
	}

	containerfile, err := c.parseContainerfile()
	if err != nil {
		return err
	}

	if err := c.processLabelsAndAnnotations(); err != nil {
		return err
	}

	if err := c.setSecretArgs(); err != nil {
		return err
	}

	prefetchResources, err := c.integrateWithPrefetch()
	if err != nil {
		return fmt.Errorf("setting up prefetch integration: %w", err)
	}

	if err := c.prepareYumReposMount(prefetchResources); err != nil {
		return fmt.Errorf("preparing yum.repos.d mount: %w", err)
	}

	if err := c.integrateWithRHSM(); err != nil {
		return fmt.Errorf("setting up RHSM integration: %w", err)
	}

	if !c.Params.SkipInjections {
		if c.Params.Target != "" {
			l.Logger.Warnf("Injecting buildinfo is not supported with --target. Skipping.")
		} else if err := c.injectBuildinfo(containerfile, c.mergedLabels, prefetchResources); err != nil {
			return fmt.Errorf("injecting buildinfo metadata: %w", err)
		}
	}

	pulledImages, err := c.prePullBaseImages(containerfile)
	if err != nil {
		return err
	}

	if err := c.verifyBaseImageArchitectures(pulledImages); err != nil {
		return err
	}

	// The auto-magical host integration breaks our explicit RHSM support, disable it.
	// Technically, we only have to disable if the user requests RHSM features,
	// but let's disable unconditionally because the magic makes builds less predictable.
	if err := c.disableRHSMHostIntegration(); err != nil {
		return fmt.Errorf("disabling RHSM host integration: %w", err)
	}

	if err := c.buildImage(); err != nil {
		return err
	}

	c.Results.ImageUrl = c.Params.OutputRef

	if c.Params.Push {
		digest, err := c.pushImage()
		if err != nil {
			return err
		}
		c.Results.Digest = digest
	}

	if c.Params.ContainerfileJsonOutput != "" {
		if err := c.writeContainerfileJson(containerfile, c.Params.ContainerfileJsonOutput); err != nil {
			return err
		}
	}
	if c.Params.ResolvedBaseImagesOutput != "" {
		if err := c.writeResolvedBaseImages(pulledImages, c.Params.ResolvedBaseImagesOutput); err != nil {
			return err
		}
	}

	if resultJson, err := c.ResultsWriter.CreateResultJson(c.Results); err == nil {
		fmt.Print(resultJson)
	} else {
		l.Logger.Errorf("failed to create results json: %s", err.Error())
		return err
	}

	return nil
}

func (c *Build) validateParams() error {
	if !common.IsImageNameValid(common.GetImageName(c.Params.OutputRef)) {
		return fmt.Errorf("output-ref '%s' is invalid", c.Params.OutputRef)
	}

	if stat, err := os.Stat(c.effectiveContextDir()); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("context directory '%s' does not exist", c.effectiveContextDir())
		}
		return fmt.Errorf("failed to stat context directory: %w", err)
	} else if !stat.IsDir() {
		return fmt.Errorf("context path '%s' is not a directory", c.effectiveContextDir())
	}

	if c.Params.Source != "" {
		resolvedSource, err := common.ResolvePath(c.Params.Source)
		if err != nil {
			return fmt.Errorf("resolving source directory: %w", err)
		}
		resolvedContext, err := common.ResolvePath(c.effectiveContextDir())
		if err != nil {
			return fmt.Errorf("resolving context directory: %w", err)
		}
		if !resolvedContext.IsRelativeTo(resolvedSource) {
			return fmt.Errorf("context directory '%s' is outside source directory '%s'", c.Params.Context, c.Params.Source)
		}
	}

	if c.Params.LegacyBuildTimestamp != "" && c.Params.SourceDateEpoch != "" {
		return fmt.Errorf("legacy-build-timestamp and source-date-epoch are mutually exclusive")
	}

	if c.Params.YumReposDTarget != "" && !filepath.IsAbs(c.Params.YumReposDTarget) {
		return fmt.Errorf("yum-repos-d-target must be an absolute path, got '%s'", c.Params.YumReposDTarget)
	}

	if c.Params.PrefetchDirCopy != "" {
		if _, err := os.Lstat(c.Params.PrefetchDirCopy); !os.IsNotExist(err) {
			return fmt.Errorf("prefetch-dir-copy must not be an existing path: %s", c.Params.PrefetchDirCopy)
		}
	}

	if c.Params.RHSMEntitlements != "" && c.Params.RHSMActivationKey != "" {
		return fmt.Errorf("rhsm-entitlements and rhsm-activation-key are mutually exclusive")
	}

	if (c.Params.RHSMActivationKey != "") != (c.Params.RHSMOrg != "") {
		return fmt.Errorf("rhsm-activation-key and rhsm-org must be used together")
	}

	if c.Params.RHSMActivationPreregister && c.Params.RHSMActivationKey == "" {
		return fmt.Errorf("rhsm-activation-preregister requires rhsm-activation-key and rhsm-org")
	}

	if c.Params.RHSMActivationMount != "" && c.Params.RHSMActivationKey == "" {
		return fmt.Errorf("rhsm-activation-mount requires rhsm-activation-key and rhsm-org")
	}

	if c.Params.RHSMActivationMount != "" && !filepath.IsAbs(c.Params.RHSMActivationMount) {
		return fmt.Errorf("rhsm-activation-mount must be an absolute path, got '%s'", c.Params.RHSMActivationMount)
	}

	if c.Params.RHSMActivationKey != "" && c.Params.RHSMActivationMount == "" && !c.Params.RHSMActivationPreregister {
		return fmt.Errorf("rhsm-activation-key requires rhsm-activation-mount or rhsm-activation-preregister")
	}

	if c.Params.RHSMMountCACerts != "" {
		validMountCACerts := map[string]bool{"always": true, "auto": true, "never": true}
		if !validMountCACerts[c.Params.RHSMMountCACerts] {
			return fmt.Errorf("rhsm-mount-ca-certs must be one of 'always', 'auto', 'never', got '%s'", c.Params.RHSMMountCACerts)
		}
	}

	if c.Params.RewriteTimestamp && c.Params.SourceDateEpoch == "" {
		// Not an error, just a warning (buildah also doesn't error for this combination of flags)
		l.Logger.Warn("RewriteTimestamp is enabled but SourceDateEpoch was not provided. Timestamps will not be re-written.")
	}

	return nil
}

func (c *Build) detectBuildahVersion() error {
	buildahVersion, err := c.CliWrappers.BuildahCli.Version()
	if err != nil {
		return fmt.Errorf("getting buildah version: %w", err)
	}
	parsedVersion, err := buildahVersion.ParseVersion()
	if err != nil {
		return fmt.Errorf("parsing buildah version: %w", err)
	}
	c.buildahVersion = buildahVersion
	c.parsedBuildahVersion = parsedVersion
	l.Logger.Debugf("Using buildah version %s", c.buildahVersion.Version)
	return nil
}

func (c *Build) detectContainerfile() error {
	source := c.Params.Source
	if source == "" {
		source = "."
	}
	containerfile, err := common.SearchDockerfile(common.DockerfileSearchOpts{
		SourceDir:  source,
		ContextDir: c.Params.Context,
		Dockerfile: c.Params.Containerfile,
	})
	if err != nil {
		return fmt.Errorf("looking for containerfile: %w", err)
	}
	if containerfile == "" {
		return fmt.Errorf("containerfile does not exist")
	}

	if c.Params.Source != "" {
		resolvedSource, err := common.ResolvePath(c.Params.Source)
		if err != nil {
			return fmt.Errorf("resolving source directory: %w", err)
		}
		resolvedContainerfile, err := common.ResolvePath(containerfile)
		if err != nil {
			return fmt.Errorf("resolving containerfile path: %w", err)
		}
		if !resolvedContainerfile.IsRelativeTo(resolvedSource) {
			return fmt.Errorf("containerfile '%s' is outside source directory '%s'", containerfile, c.Params.Source)
		}
	}

	c.containerfilePath = containerfile
	return nil
}

func (c *Build) setSecretArgs() error {
	secretDirs, err := parseSecretDirs(c.Params.SecretDirs)
	if err != nil {
		return fmt.Errorf("parsing --secret-dirs: %w", err)
	}
	buildahSecrets, err := c.processSecretDirs(secretDirs)
	if err != nil {
		return fmt.Errorf("processing --secret-dirs: %w", err)
	}
	c.buildahSecrets = buildahSecrets
	return nil
}

type secretDir struct {
	src      string
	name     string
	optional bool
}

func parseSecretDirs(secretDirArgs []string) ([]secretDir, error) {
	var secretDirs []secretDir

	for _, arg := range secretDirArgs {
		secretDir := secretDir{}
		keyValues := strings.Split(arg, ",")

		for _, kv := range keyValues {
			key, value, hasSep := strings.Cut(kv, "=")
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)

			if !hasSep {
				value = key
				key = "src"
			}

			switch key {
			case "src":
				secretDir.src = value
			case "name":
				secretDir.name = value
			case "optional":
				switch value {
				case "true":
					secretDir.optional = true
				case "false":
					secretDir.optional = false
				default:
					return nil, fmt.Errorf("invalid argument: optional=%s (expected true|false)", value)
				}
			default:
				return nil, fmt.Errorf("invalid attribute: %s", key)
			}
		}

		secretDirs = append(secretDirs, secretDir)
	}

	return secretDirs, nil
}

// processSecretDirs processes secret directories and returns buildah --secret arguments.
func (c *Build) processSecretDirs(secretDirs []secretDir) ([]cliWrappers.BuildahSecret, error) {
	var buildahSecrets []cliWrappers.BuildahSecret
	usedIDs := make(map[string]bool)

	for _, secretDir := range secretDirs {
		idPrefix := secretDir.name
		if idPrefix == "" {
			idPrefix = filepath.Base(secretDir.src)
		}

		entries, err := os.ReadDir(secretDir.src)
		if err != nil {
			if os.IsNotExist(err) && secretDir.optional {
				l.Logger.Debugf("secret directory %s doesn't exist but is marked optional, skipping", secretDir.src)
				continue
			}
			return nil, fmt.Errorf("failed to read secret directory %s: %w", secretDir.src, err)
		}

		for _, entry := range entries {
			isFile, err := isRegular(entry, secretDir.src)
			if err != nil {
				return nil, err
			}
			if !isFile {
				continue
			}

			filename := entry.Name()
			fullID := filepath.Join(idPrefix, filename)

			// Check for ID conflicts
			if usedIDs[fullID] {
				return nil, fmt.Errorf("duplicate secret ID '%s': ensure unique basename/filename combinations", fullID)
			}
			usedIDs[fullID] = true

			secretPath := filepath.Join(secretDir.src, filename)
			buildahSecrets = append(
				buildahSecrets, cliWrappers.BuildahSecret{Src: secretPath, Id: fullID},
			)

			l.Logger.Infof("Adding secret %s to the build, available with 'RUN --mount=type=secret,id=%s'", fullID, fullID)
		}
	}

	return buildahSecrets, nil
}

func isRegular(entry os.DirEntry, dir string) (bool, error) {
	t := entry.Type()
	if t.IsRegular() {
		return true, nil
	}
	if t == os.ModeSymlink {
		path := filepath.Join(dir, entry.Name())
		stat, err := os.Stat(path)
		if err != nil {
			return false, fmt.Errorf("failed to stat %s: %w", path, err)
		}
		return stat.Mode().IsRegular(), nil
	}
	return false, nil
}

type prefetchResources struct {
	outputDir string
	envFile   string
	yumReposD string
	sbomFile  string
}

func findPrefetchResources(prefetchDir string) (*prefetchResources, error) {
	var resources prefetchResources

	outputDir := filepath.Join(prefetchDir, "output")
	if _, err := os.Lstat(outputDir); err == nil {
		l.Logger.Debugf("Found prefetched dependencies: %s", outputDir)
		resources.outputDir = outputDir
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	// For backwards compatibility, also look for cachi2.env (but prefetch.env is preferred)
	for _, envfile := range []string{"prefetch.env", "cachi2.env"} {
		envfilePath := filepath.Join(prefetchDir, envfile)
		if _, err := os.Lstat(envfilePath); err == nil {
			l.Logger.Debugf("Found prefetch env file: %s", envfilePath)
			resources.envFile = envfilePath
			break
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}

	currentArch := goArchToRpmArch(runtime.GOARCH)

	reposD := filepath.Join(prefetchDir, "output", "deps", "rpm", currentArch, "repos.d")
	if _, err := os.Lstat(reposD); err == nil {
		l.Logger.Debugf("Found prefetch yum repos for current architecture: %s", reposD)
		resources.yumReposD = reposD
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	sbomFile := filepath.Join(prefetchDir, "output", "bom.json")
	if _, err := os.Lstat(sbomFile); err == nil {
		l.Logger.Debugf("Found prefetch SBOM: %s", sbomFile)
		resources.sbomFile = sbomFile
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	return &resources, nil
}

func (c *Build) integrateWithPrefetch() (*prefetchResources, error) {
	if c.Params.PrefetchDir == "" {
		return nil, nil
	}

	l.Logger.Info("Setting up prefetch integration...")

	prefetchDirCopy, err := c.copyPrefetchDir()
	if err != nil {
		return nil, fmt.Errorf("copying prefetch resources: %w", err)
	}

	resources, err := findPrefetchResources(prefetchDirCopy)
	if err != nil {
		return nil, fmt.Errorf("looking for prefetch resources in %s: %w", prefetchDirCopy, err)
	}

	if resources.outputDir != "" {
		outputMountPath := c.Params.PrefetchOutputMount
		if outputMountPath == "" {
			outputMountPath = defaultPrefetchOutputMount
		}

		c.buildahVolumes = append(c.buildahVolumes, cliWrappers.BuildahVolume{
			HostDir:      resources.outputDir,
			ContainerDir: outputMountPath,
			Options:      "z",
		})
	}

	if resources.envFile != "" {
		envMountPath := c.Params.PrefetchEnvMount
		if envMountPath == "" {
			envMountPath = defaultPrefetchEnvMount
		}

		c.buildahVolumes = append(c.buildahVolumes, cliWrappers.BuildahVolume{
			HostDir:      resources.envFile,
			ContainerDir: envMountPath,
			Options:      "z",
		})

		l.Logger.Debug("Modifying containerfile to apply prefetch env")
		if err := c.injectPrefetchEnvToContainerfile(envMountPath); err != nil {
			return nil, fmt.Errorf("modifying containerfile to apply prefetch env: %w", err)
		}
	}

	return resources, nil
}

// Copy the relevant resources from the prefetch dir to a temporary directory.
// Note that this temporary directory can't go to /tmp (and by extension, can't go tempWorkdir)
// because the size of the prefetched dependencies is often too large for a tmpfs.
//
// Reasons why we need to copy:
//   - All the prefetch resources need to be rw to everyone. Any user in the containerfile
//     may need to write to GOCACHE, for example. We can't expect to always have ownership of
//     the prefetch dir, so we can't chmod it directly. But we can chmod a copy.
//   - If the RUN instructions in the containerfile modify the content of the prefetch dir,
//     this should not affect the original host directory. Mounting with the O option (overlay)
//     could solve this, but doesn't seem to work for rootless buildah.
//   - The build shouldn't mount the entire prefetch tree, because it may contain RPMs for arches
//     other than the current arch, and we don't want to make unnecessary content available to
//     the build (this content may be unaccounted for in the SBOM). This function only copies
//     the relevant deps directories.
//
// Ideally, the temporary directory should be on the same filesystem as the original prefetch dir,
// which allows io.Copy to use CoW copies on some filesystems like btrfs or xfs. When successful,
// this avoids duplicating the potentially massive amount of underlying data.
// This function tries to achieve that by copying to a subdirectory of the original prefetch-dir,
// but the user can override this with --prefetch-dir-copy in case the prefetch dir is not writable.
func (c *Build) copyPrefetchDir() (string, error) {
	prefetchDir := c.Params.PrefetchDir
	prefetchDirCopy := c.Params.PrefetchDirCopy

	if prefetchDirCopy != "" {
		err := os.Mkdir(prefetchDirCopy, 0755)
		if err != nil {
			return "", err
		}
	} else {
		// Default to a subdirectory of the original prefetch dir to guarantee same filesystem
		pdcopy, err := os.MkdirTemp(prefetchDir, "copy-*")
		if err != nil {
			return "", err
		}
		prefetchDirCopy = pdcopy
	}
	c.tempFilesOutsideWorkdir = append(c.tempFilesOutsideWorkdir, prefetchDirCopy)

	l.Logger.Debugf("Copying prefetch resources to %s", prefetchDirCopy)

	currentArch := goArchToRpmArch(runtime.GOARCH)
	// Clean the filepaths, we do string comparisons below
	prefetchDir = filepath.Clean(prefetchDir)
	prefetchDirCopy = filepath.Clean(prefetchDirCopy)

	err := filepath.WalkDir(prefetchDir, func(srcPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip the root, we've already created the destination directory
		if srcPath == prefetchDir {
			return nil
		}
		// If we find the target dir while walking the source dir, don't recurse into it
		if srcPath == prefetchDirCopy {
			l.Logger.Debugf("Skipping %s, it's the target dir", srcPath)
			return filepath.SkipDir
		}

		relPath, err := filepath.Rel(prefetchDir, srcPath)
		if err != nil {
			// probably impossible
			return fmt.Errorf("failed to make prefetchDir subpath relative to prefetchDir: %w", err)
		}

		// Don't recurse into output/deps/rpm/${arch} directories unless the arch matches ours
		isRPM := filepath.Dir(relPath) == filepath.Join("output", "deps", "rpm")
		if isRPM && filepath.Base(srcPath) != currentArch {
			l.Logger.Debugf("Skipping %s, does not match the current architecture (%s)", srcPath, currentArch)
			return filepath.SkipDir
		}

		dstPath := filepath.Join(prefetchDirCopy, relPath)

		switch d.Type() {
		case os.ModeDir:
			return os.Mkdir(dstPath, 0755)
		case os.ModeSymlink:
			target, err := os.Readlink(srcPath)
			if err != nil {
				return err
			}
			return os.Symlink(target, dstPath)
		case 0: // regular
			return copyFile(srcPath, dstPath)
		default:
			return fmt.Errorf("unsupported file %s, type bits: %#o", srcPath, d.Type())
		}
	})
	if err != nil {
		return "", err
	}

	// All containerfile users need write permissions
	if err := chmodAddRWX(prefetchDirCopy); err != nil {
		return "", fmt.Errorf("adding +rwX: %w", err)
	}

	return prefetchDirCopy, nil
}

// Copy srcPath to dstPath, preserving permissions.
func copyFile(srcPath, dstPath string) (err error) {
	info, err := os.Stat(srcPath)
	if err != nil {
		return err
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer func() {
		if e := src.Close(); e != nil && err == nil {
			err = e
		}
	}()

	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}

	_, err = io.Copy(dst, src)
	if err != nil {
		// we already have an error and want to return that one
		_ = dst.Close()
		return fmt.Errorf("copy %s to %s: %w", srcPath, dstPath, err)
	}

	return dst.Close()
}

// Modifies RUN instructions in the Containerfile to source the env file at the beginning,
// after any options like --mount. Skips exec-form RUN instructions and bare heredocs
// ('RUN sh <<EOF' is supported, 'RUN <<EOF' isn't).
func (c *Build) injectPrefetchEnvToContainerfile(envMountPath string) error {
	if err := c.ensureContainerfileCopied(); err != nil {
		return err
	}

	content, err := os.ReadFile(c.containerfileCopyPath)
	if err != nil {
		return fmt.Errorf("reading containerfile copy: %w", err)
	}

	injector := dfeditor.RunInjector{OnUnsupported: func(lineno int, err error) {
		switch {
		case errors.Is(err, dfeditor.ErrRunNoOp):
			l.Logger.Warnf("Applying prefetch env: skipping RUN instruction on line %d, appears effectively empty", lineno)
		case errors.Is(err, dfeditor.ErrRunHeredoc):
			l.Logger.Warnf("Applying prefetch env: skipping unsupported RUN instruction on line %d (heredoc). "+
				"Please specify the interpreter explicitly (e.g. '/bin/sh <<EOF' instead of just '<<EOF').", lineno)
		case errors.Is(err, dfeditor.ErrRunExec):
			l.Logger.Warnf("Applying prefetch env: skipping unsupported RUN instruction on line %d (exec form). "+
				"Please use the shell form instead if possible (not a JSON array).", lineno)
		default:
			l.Logger.Warnf("Applying prefetch.env: skipping RUN instruction on line %d due to unexpected error: %s", lineno, err)
		}
	}}

	injection := fmt.Sprintf(". %s && \\\n    ", cliWrappers.ShellQuote(envMountPath))

	result, err := injector.Inject(string(content), injection)
	if err != nil {
		return fmt.Errorf("modifying containerfile to apply prefetch env: %w", err)
	}

	if err := os.WriteFile(c.containerfileCopyPath, []byte(result), 0644); err != nil {
		return fmt.Errorf("writing modified containerfile: %w", err)
	}
	return nil
}

// Copies regular files from all yum-repos-d-sources to a subdirectory in the tempWorkdir.
// On filename conflict, the file found later replaces the one found earlier.
//
// Adds a buildahVolumes mount that mounts the subdirectory at yum-repos-d-target.
func (c *Build) prepareYumReposMount(prefetchResources *prefetchResources) error {
	reposDSources := c.Params.YumReposDSources
	if prefetchResources != nil && prefetchResources.yumReposD != "" {
		reposDSources = append(reposDSources, prefetchResources.yumReposD)
	}
	if len(reposDSources) == 0 {
		return nil
	}

	if err := c.ensureTempWorkdirExists(); err != nil {
		return err
	}

	mergedDir := filepath.Join(c.tempWorkdir, "yum.repos.d")
	// For backwards compatibility, make the directory rwx to everyone.
	// This enables any user inside the containerfile to write to the mount point.
	if err := os.Mkdir(mergedDir, 0777); err != nil {
		return fmt.Errorf("creating yum.repos.d/ in temporary workdir: %w", err)
	}

	seen := make(map[string]string) // filename -> source directory that provided it

	for _, srcDir := range reposDSources {
		entries, err := os.ReadDir(srcDir)
		if err != nil {
			return fmt.Errorf("reading yum.repos.d source %s: %w", srcDir, err)
		}

		for _, entry := range entries {
			if !entry.Type().IsRegular() {
				// Also skips symlinks, there's no use for symlinks in yum.repos.d.
				// Either they would point outside the directory, and we don't even want to allow that,
				// or to a file in the same directory, duplicating it, which has no effect on dnf.
				l.Logger.Warnf("yum.repos.d: skipping %s, not a regular file", filepath.Join(srcDir, entry.Name()))
				continue
			}

			filename := entry.Name()
			if prev, ok := seen[filename]; ok {
				l.Logger.Warnf("yum.repos.d: %s from %s overwrites the one from %s", filename, srcDir, prev)
			}
			seen[filename] = srcDir

			srcPath := filepath.Join(srcDir, filename)
			dstPath := filepath.Join(mergedDir, filename)

			content, err := os.ReadFile(srcPath)
			if err != nil {
				return fmt.Errorf("reading %s: %w", srcPath, err)
			}
			if err := os.WriteFile(dstPath, content, 0666); err != nil {
				return fmt.Errorf("writing %s: %w", dstPath, err)
			}

			l.Logger.Infof("yum.repos.d: added %s from %s", filename, srcDir)
		}
	}

	// We already attempt to use 777 for the directory and 666 for the files,
	// but if we're inside a container, umask will likely strip the write bits for group and other.
	// Chmod again to fix the permissions.
	if err := chmodAddRWX(mergedDir); err != nil {
		return fmt.Errorf("fixing yum.repos.d permissions: %w", err)
	}

	target := c.Params.YumReposDTarget
	if target == "" {
		target = "/etc/yum.repos.d"
	}

	c.buildahVolumes = append(c.buildahVolumes, cliWrappers.BuildahVolume{
		HostDir:      mergedDir,
		ContainerDir: target,
		Options:      "z",
	})

	return nil
}

// Recursively adds read-write permissions, execute permission as well if the file
// is a directory or has at least one execute bit already set (equivalent to 'chmod -R +rwX').
// Skips symlinks.
func chmodAddRWX(rootDir string) error {
	return filepath.WalkDir(rootDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		perm := info.Mode().Perm()
		perm |= 0666 // +rw for user, group, other
		if entry.IsDir() || info.Mode()&0111 != 0 {
			perm |= 0111 // +x for user, group, other
		}
		return os.Chmod(path, perm)
	})
}

func (c *Build) integrateWithRHSM() error {
	if c.Params.RHSMEntitlements == "" && c.Params.RHSMActivationKey == "" {
		return nil
	}

	if c.Params.RHSMActivationPreregister {
		if err := c.registerRHSM(); err != nil {
			return fmt.Errorf("registering with subscription-manager: %w", err)
		}
		c.registeredWithRHSM = true
	}

	rhsm, err := c.gatherRHSMresources()
	if err != nil {
		return err
	}

	maybeMount := func(src string, dest string) {
		if src != "" {
			volume := cliWrappers.BuildahVolume{HostDir: src, ContainerDir: dest, Options: "z"}
			c.buildahVolumes = append(c.buildahVolumes, volume)
		}
	}
	maybeMount(rhsm.entitlementCerts, "/etc/pki/entitlement")
	maybeMount(rhsm.consumerCerts, "/etc/pki/consumer")
	maybeMount(rhsm.caCerts, "/etc/rhsm/ca")
	maybeMount(rhsm.activationSecrets, c.Params.RHSMActivationMount)

	return nil
}

func (c *Build) registerRHSM() error {
	key, err := os.ReadFile(c.Params.RHSMActivationKey)
	if err != nil {
		return err
	}
	org, err := os.ReadFile(c.Params.RHSMOrg)
	if err != nil {
		return err
	}

	params := &cliWrappers.SubscriptionManagerRegisterParams{
		Org:           strings.TrimSpace(string(org)),
		ActivationKey: strings.TrimSpace(string(key)),
		Force:         true,
	}
	return c.CliWrappers.SubscriptionManager.Register(params)
}

type rhsmResources struct {
	entitlementCerts  string // directory with files from /etc/pki/entitlement
	consumerCerts     string // directory with files from /etc/pki/consumer
	caCerts           string // directory with files from /etc/rhsm/ca
	activationSecrets string // directory with 'activationkey' and 'org' files
}

// Find the files relevant for RHSM integration and copy them to subdirectories of the tempWorkdir.
// Copying is necessary so that, when we mount them into the build, the build doesn't have direct
// access to modify the original host files.
func (c *Build) gatherRHSMresources() (*rhsmResources, error) {
	var rhsm rhsmResources

	if err := c.ensureTempWorkdirExists(); err != nil {
		return nil, err
	}

	mkWorkdir := func(dirname string, outPath *string) error {
		path := filepath.Join(c.tempWorkdir, dirname)
		if err := os.Mkdir(path, 0755); err != nil {
			return fmt.Errorf("creating temporary directory for RHSM files: %w", err)
		}
		*outPath = path
		return nil
	}

	if c.Params.RHSMEntitlements != "" {
		if err := mkWorkdir("rhsm-entitlement", &rhsm.entitlementCerts); err != nil {
			return nil, err
		}
		if err := copyRegularFiles(c.Params.RHSMEntitlements, rhsm.entitlementCerts); err != nil {
			return nil, fmt.Errorf("copying entitlements: %w", err)
		}
	} else if c.Params.RHSMActivationKey != "" {
		// Always create the entitlement and consumer dirs. Even if we didn't pre-pregister,
		// we still want to mount empty dirs over /etc/pki/{entitlement,consumer} so that,
		// if the build runs 'subscription-manager register', the outputs go to the volume mount
		// instead of going to the container filesystem (the outputs are secrets).
		if err := mkWorkdir("rhsm-entitlement", &rhsm.entitlementCerts); err != nil {
			return nil, err
		}
		if err := mkWorkdir("rhsm-consumer", &rhsm.consumerCerts); err != nil {
			return nil, err
		}

		if c.Params.RHSMActivationPreregister {
			if err := copyRegularFiles(c.hostEntitlements, rhsm.entitlementCerts); err != nil {
				return nil, fmt.Errorf("copying %s: %w", c.hostEntitlements, err)
			}
			if err := copyRegularFiles(c.hostConsumerCerts, rhsm.consumerCerts); err != nil {
				return nil, fmt.Errorf("copying %s: %w", c.hostConsumerCerts, err)
			}
		}

		if c.Params.RHSMActivationMount != "" {
			if err := mkWorkdir("rhsm-activation", &rhsm.activationSecrets); err != nil {
				return nil, err
			}
			activationkey := filepath.Join(rhsm.activationSecrets, "activationkey")
			if err := copyFile(c.Params.RHSMActivationKey, activationkey); err != nil {
				return nil, fmt.Errorf("copying activation key file: %w", err)
			}
			org := filepath.Join(rhsm.activationSecrets, "org")
			if err := copyFile(c.Params.RHSMOrg, org); err != nil {
				return nil, fmt.Errorf("copying org file: %w", err)
			}
		}
	}

	if c.shouldMountRHSMcaCerts() {
		if _, err := os.Stat(c.hostRHSMcaCerts); err == nil {
			if err := mkWorkdir("rhsm-ca-certs", &rhsm.caCerts); err != nil {
				return nil, err
			}
			if err := copyRegularFiles(c.hostRHSMcaCerts, rhsm.caCerts); err != nil {
				return nil, fmt.Errorf("copying RHSM CA certificates: %w", err)
			}
		} else if os.IsNotExist(err) {
			if c.Params.RHSMMountCACerts == "always" {
				return nil, fmt.Errorf("rhsm-mount-ca-certs=always, but %s doesn't exist", c.hostRHSMcaCerts)
			} else {
				l.Logger.Warnf("Couldn't mount RHSM CA certificates into the build, %s doesn't exist. "+
					"This may not be a problem if the build already has the certificates installed, proceeding.",
					c.hostRHSMcaCerts)
			}
		} else {
			return nil, fmt.Errorf("checking %s existence: %w", c.hostRHSMcaCerts, err)
		}
	}

	return &rhsm, nil
}

func (c *Build) shouldMountRHSMcaCerts() bool {
	switch c.Params.RHSMMountCACerts {
	case "always":
		return true
	case "never":
		return false
	default:
		isSelfRegistration := c.Params.RHSMActivationKey != "" && !c.Params.RHSMActivationPreregister
		return !isSelfRegistration
	}
}

func copyRegularFiles(srcDir, dstDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		isFile, err := isRegular(entry, srcDir)
		if err != nil {
			return err
		}
		if !isFile {
			continue
		}

		srcPath := filepath.Join(srcDir, entry.Name())
		dstPath := filepath.Join(dstDir, entry.Name())
		if err := copyFile(srcPath, dstPath); err != nil {
			return err
		}
	}
	return nil
}

func (c *Build) parseContainerfile() (*dockerfile.Dockerfile, error) {
	l.Logger.Debugf("Parsing Containerfile: %s", c.containerfilePath)

	containerfile, err := dockerfile.Parse(c.containerfilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", c.containerfilePath, err)
	}

	envs := processKeyValueEnvs(c.Params.Envs)

	argExp, err := c.createBuildArgExpander()
	if err != nil {
		return nil, fmt.Errorf("failed to process build args: %w", err)
	}

	containerfile.InjectEnv(envs)
	containerfile.Expand(argExp)
	return containerfile, nil
}

func (c *Build) createBuildArgExpander() (dockerfile.SingleWordExpander, error) {
	// Define built-in ARG variables
	// See https://docs.docker.com/build/building/variables/#multi-platform-build-arguments
	platform := platforms.Normalize(platforms.DefaultSpec())
	args := map[string]string{
		// We current don't explicitly expose the --platform flag, so the TARGET* values always
		// match the BUILD* values. If we add --platform handling, we would want to respect it here.
		"TARGETPLATFORM": platforms.Format(platform),
		"TARGETOS":       platform.OS,
		"TARGETARCH":     platform.Architecture,
		"TARGETVARIANT":  platform.Variant,
		"BUILDPLATFORM":  platforms.Format(platform),
		"BUILDOS":        platform.OS,
		"BUILDARCH":      platform.Architecture,
		"BUILDVARIANT":   platform.Variant,
	}

	// Load from --build-args-file, can override built-in args
	if c.Params.BuildArgsFile != "" {
		fileArgs, err := buildargs.ParseBuildArgFile(c.Params.BuildArgsFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read build args file: %w", err)
		}
		maps.Copy(args, fileArgs)
	}

	// CLI --build-args take precedence over everything else
	cliArgs := processKeyValueEnvs(c.Params.BuildArgs)
	maps.Copy(args, cliArgs)

	// Return the kind of "expander" function expected by the dockerfile-json API
	// (takes the name of a build arg, returns the value or error for undefined build args)
	argExp := func(word string) (string, error) {
		if value, ok := args[word]; ok {
			return value, nil
		}
		return "", fmt.Errorf("not defined: $%s", word)
	}
	return argExp, nil
}

// Parse an array of key[=value] args. If '=' is missing, look up the value in
// environment variables. This is how buildah handles --build-arg and --env values.
func processKeyValueEnvs(args []string) map[string]string {
	values := make(map[string]string)
	for _, arg := range args {
		key, value, hasValue := strings.Cut(arg, "=")
		if hasValue {
			values[key] = value
		} else if valueFromEnv, ok := os.LookupEnv(key); ok {
			values[key] = valueFromEnv
		}
	}
	return values
}

// Prepends default labels and annotations to the user-provided values.
// User-provided values override defaults via buildah's "last value wins" behavior.
//
// The default annotations are primarily based on the OCI annotation spec:
// https://specs.opencontainers.org/image-spec/annotations/
//
// Note that we also add them as labels, not just annotations. When distributing images
// in the docker format rather than the OCI format, annotations disappear, so the information
// is at least preserved as labels.
//
// In addition to the OCI annotations (and labels), if AddLegacyLabels is enabled,
// adds labels based on https://github.com/projectatomic/ContainerApplicationGenericLabels.
func (c *Build) processLabelsAndAnnotations() error {
	var defaultLabels []string
	var defaultAnnotations []string

	buildTimeStr, err := c.getBuildTimeRFC3339()
	if err != nil {
		return fmt.Errorf("determining build timestamp: %w", err)
	}
	ociCreated := "org.opencontainers.image.created=" + buildTimeStr
	defaultAnnotations = append(defaultAnnotations, ociCreated)
	defaultLabels = append(defaultLabels, ociCreated)

	if c.Params.ImageSource != "" {
		ociSource := "org.opencontainers.image.source=" + c.Params.ImageSource

		defaultAnnotations = append(defaultAnnotations, ociSource)
		defaultLabels = append(defaultLabels, ociSource)
	}

	if c.Params.ImageRevision != "" {
		ociRevision := "org.opencontainers.image.revision=" + c.Params.ImageRevision

		defaultAnnotations = append(defaultAnnotations, ociRevision)
		defaultLabels = append(defaultLabels, ociRevision)
	}

	if c.Params.QuayImageExpiresAfter != "" {
		defaultLabels = append(defaultLabels, "quay.expires-after="+c.Params.QuayImageExpiresAfter)
	}

	if c.Params.AddLegacyLabels {
		defaultLabels = append(defaultLabels, "build-date="+buildTimeStr)

		arch := goArchToRpmArch(runtime.GOARCH)
		defaultLabels = append(defaultLabels, "architecture="+arch)

		if c.Params.ImageSource != "" {
			defaultLabels = append(defaultLabels, "vcs-url="+c.Params.ImageSource)
		}

		if c.Params.ImageRevision != "" {
			defaultLabels = append(defaultLabels, "vcs-ref="+c.Params.ImageRevision)
		}

		if c.Params.ImageSource != "" || c.Params.ImageRevision != "" {
			// We don't know if it's git, but this label serves no purpose other than
			// to appease the default Red Hat label policy, so it doesn't really matter.
			// https://github.com/release-engineering/rhtap-ec-policy/blob/25b163398303105a539998f1a276f176bf3384b2/data/rule_data.yml#L103
			defaultLabels = append(defaultLabels, "vcs-type=git")
		}
	}

	mergedLabels := append(defaultLabels, c.Params.Labels...)

	mergedAnnotations := defaultAnnotations
	if c.Params.AnnotationsFile != "" {
		fileAnnotations, err := parseAnnotationsFile(c.Params.AnnotationsFile)
		if err != nil {
			return fmt.Errorf("parsing annotations file: %w", err)
		}
		// --annotations-file takes precedence over defaults
		mergedAnnotations = append(mergedAnnotations, fileAnnotations...)
	}
	// --annotations take precedence over --annotations-file
	mergedAnnotations = append(mergedAnnotations, c.Params.Annotations...)

	c.mergedLabels = mergedLabels
	c.mergedAnnotations = mergedAnnotations
	return nil
}

func (c *Build) getBuildTimeRFC3339() (string, error) {
	var buildTime time.Time
	if c.Params.SourceDateEpoch != "" {
		timestamp, err := strconv.ParseInt(c.Params.SourceDateEpoch, 10, 64)
		if err != nil {
			return "", fmt.Errorf("parsing source-date-epoch: %w", err)
		}
		buildTime = time.Unix(timestamp, 0).UTC()
	} else if c.Params.LegacyBuildTimestamp != "" {
		timestamp, err := strconv.ParseInt(c.Params.LegacyBuildTimestamp, 10, 64)
		if err != nil {
			return "", fmt.Errorf("parsing legacy-build-timestamp: %w", err)
		}
		buildTime = time.Unix(timestamp, 0).UTC()
	} else {
		buildTime = time.Now().UTC()
	}
	return buildTime.Format(time.RFC3339), nil
}

func parseAnnotationsFile(filePath string) ([]string, error) {
	annotations, err := buildargs.ParseBuildArgFile(filePath)
	if err != nil {
		return nil, err
	}
	annotationStrings := []string{}
	for k, v := range annotations {
		annotationStrings = append(annotationStrings, k+"="+v)
	}
	slices.Sort(annotationStrings)
	return annotationStrings, nil
}

// Convert a supported GOARCH value to the corresponding RPM architecture name.
func goArchToRpmArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return goarch
	}
}

// Injects metadata into the image at /usr/share/buildinfo.
//
// Injected files:
// - labels.json: contains the labels of the resulting image, needed for the Clair scanning tool
// - content-sets.json: contains the names of RPM repositories used for prefetching, needed for Clair
func (c *Build) injectBuildinfo(df *dockerfile.Dockerfile, userLabels []string, prefetchResources *prefetchResources) error {
	// Create buildinfo directory in the temporary workdir and add it as a --build-context
	buildinfoDir, err := c.createBuildinfoDir()
	if err != nil {
		return fmt.Errorf("creating buildinfo dir: %w", err)
	}
	c.buildinfoBuildContext = &cliWrappers.BuildahBuildContext{Name: ".konflux-buildinfo", Location: buildinfoDir}

	// Create labels.json in buildinfo dir
	labels, err := c.determineFinalLabels(df, userLabels)
	if err != nil {
		return fmt.Errorf("determining labels for labels.json: %w", err)
	}
	if err := writeBuildinfoJSON(buildinfoDir, labels, "labels.json"); err != nil {
		return fmt.Errorf("writing labels.json to buildinfo dir: %w", err)
	}
	l.Logger.Info("Injecting buildinfo: added labels.json")

	if err := c.ensureContainerfileCopied(); err != nil {
		return err
	}

	if prefetchResources != nil && prefetchResources.sbomFile != "" {
		// Create content-sets.json in buildinfo dir
		contentSets, err := determineContentSets(prefetchResources.sbomFile)
		if err != nil {
			return fmt.Errorf("determining content sets for content-sets.json: %w", err)
		}
		if err := writeBuildinfoJSON(buildinfoDir, contentSets, "content-sets.json"); err != nil {
			return fmt.Errorf("writing content-sets.json to buildinfo dir: %w", err)
		}
		l.Logger.Info("Injecting buildinfo: added content-sets.json")
	} else {
		l.Logger.Info("Injecting buildinfo: no prefetch SBOM found, not adding content-sets.json")
	}

	appendLines := []string{"COPY --from=.konflux-buildinfo . /usr/share/buildinfo/"}
	if c.Params.IncludeLegacyBuildinfoPath {
		appendLines = append(appendLines, "COPY --from=.konflux-buildinfo . /root/buildinfo/")
	}
	for _, line := range appendLines {
		l.Logger.Debugf("Appending to containerfile: %s", line)
	}
	// prepend a newline in case the input containerfile doesn't end with one
	appendContent := "\n" + strings.Join(appendLines, "\n") + "\n"

	if err := appendToFile(c.containerfileCopyPath, appendContent); err != nil {
		return fmt.Errorf("writing to containerfile copy: %w", err)
	}

	return nil
}

func (c *Build) createBuildinfoDir() (string, error) {
	if err := c.ensureTempWorkdirExists(); err != nil {
		return "", err
	}

	buildinfoDir := filepath.Join(c.tempWorkdir, "buildinfo")
	if err := os.Mkdir(buildinfoDir, 0755); err != nil {
		return "", err
	}

	return buildinfoDir, nil
}

func writeBuildinfoJSON(buildinfoDir string, value any, filename string) error {
	// Note: json.MarshalIndent sorts map keys, so the output is deterministic.
	// This is crucial for reproducibility.
	jsonContent, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling to JSON: %w", err)
	}

	filePath := filepath.Join(buildinfoDir, filename)
	// For backwards compatibility, buildinfo files should be readable to all
	if err := os.WriteFile(filePath, append(jsonContent, '\n'), 0644); err != nil {
		return err
	}

	return nil
}

func appendToFile(filePath, content string) error {
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(content); err != nil {
		// we already have an error and want to return that one
		_ = file.Close()
		return err
	}
	return file.Close()
}

// Collect all repository_id qualifiers from the purls in the prefetch SBOM (the "content sets").
// Return them wrapped in a "content manifest" (a deprecated format still needed by Clair).
// https://github.com/konflux-ci/buildah-container/blob/5fd8a4b1163079c7978e79100ec51b41504e0f20/scripts/icm-injection-scripts/inject-icm.sh
func determineContentSets(prefetchSbomFile string) (map[string]any, error) {
	sbomContent, err := os.ReadFile(prefetchSbomFile)
	if err != nil {
		return nil, fmt.Errorf("reading prefetch SBOM: %w", err)
	}

	var sbom struct {
		// CycloneDX
		BomFormat  string `json:"bomFormat"`
		Components []struct {
			Purl string `json:"purl"`
		} `json:"components"`
		// SPDX
		Packages []struct {
			ExternalRefs []struct {
				ReferenceType    string `json:"referenceType"`
				ReferenceLocator string `json:"referenceLocator"`
			} `json:"externalRefs"`
		} `json:"packages"`
	}

	if err := json.Unmarshal(sbomContent, &sbom); err != nil {
		return nil, fmt.Errorf("unmarshalling prefetch SBOM: %w", err)
	}

	contentSets := make(map[string]struct{})

	processPurl := func(purl string) {
		parsedPurl, err := packageurl.FromString(purl)
		if err != nil {
			// For compatibility with the original script, don't abort on broken purls
			l.Logger.Warnf("Constructing content-sets.json: failed to parse %s as purl, skipping: %s", purl, err)
			return
		}
		for _, qualifier := range parsedPurl.Qualifiers {
			if qualifier.Key == "repository_id" {
				contentSets[qualifier.Value] = struct{}{}
			}
		}
	}

	if sbom.BomFormat == "CycloneDX" {
		for _, component := range sbom.Components {
			processPurl(component.Purl)
		}
	} else {
		for _, pkg := range sbom.Packages {
			for _, ref := range pkg.ExternalRefs {
				if ref.ReferenceType == "purl" {
					processPurl(ref.ReferenceLocator)
				}
			}
		}
	}

	// https://github.com/konflux-ci/buildah-container/blob/5fd8a4b1163079c7978e79100ec51b41504e0f20/scripts/icm-injection-scripts/inject-icm.sh#L54
	contentManifest := map[string]any{
		"metadata": map[string]any{
			"icm_version":       1,
			"icm_spec":          "https://raw.githubusercontent.com/containerbuildsystem/atomic-reactor/master/atomic_reactor/schemas/content_manifest.json",
			"image_layer_index": 0,
		},
		"from_dnf_hint": true,
		"content_sets":  slices.Sorted(maps.Keys(contentSets)),
	}
	return contentManifest, nil
}

func (c *Build) determineFinalLabels(df *dockerfile.Dockerfile, userLabels []string) (map[string]string, error) {
	labels := make(map[string]string)

	var baseImage string
	var containerfileLabels map[string]string
	if c.Params.InheritLabels {
		baseImage, containerfileLabels = processUntilBaseStage(df)
	} else if df != nil && len(df.Stages) > 0 {
		// Label inheritance disabled, don't get base image labels
		// or labels from any stage except the target stage
		containerfileLabels = getStageLabels(df.Stages[len(df.Stages)-1])
	}

	// Base image labels
	if baseImage != "" {
		if isPullableImage(baseImage) {
			baseImageLabels, err := c.getImageLabels(baseImage)
			if err != nil {
				return nil, fmt.Errorf("getting base image labels: %w", err)
			}
			maps.Copy(labels, baseImageLabels)
		} else {
			l.Logger.Warnf("Injecting labels.json: ignoring base image labels due to unsupported transport: %s", baseImage)
		}
	} // else base image is FROM scratch => no labels

	// Containerfile labels
	maps.Copy(labels, containerfileLabels)

	// User-provided labels
	for _, label := range userLabels {
		key, value, _ := strings.Cut(label, "=")
		labels[key] = value
	}

	// Automatic buildah version label (highest precedence)
	// Only injected if --source-date-epoch (or --timestamp, which we do not expose) are not used.
	// See https://www.mankier.com/1/buildah-build#--identity-label.
	if c.Params.SourceDateEpoch == "" {
		labels["io.buildah.version"] = c.buildahVersion.Version
	}

	return labels, nil
}

// Resolves the base stage of the final stage by following FROM references
// through intermediate stages. Collects LABELs from each stage in the chain.
// Returns the base image for the base stage and the collected labels.
func processUntilBaseStage(df *dockerfile.Dockerfile) (string, map[string]string) {
	if df == nil || len(df.Stages) == 0 {
		return "", nil
	}

	stage := df.Stages[len(df.Stages)-1]
	stageChain := []*dockerfile.Stage{}

	for stage != nil {
		stageChain = append(stageChain, stage)
		if stage.From.Stage != nil {
			stage = df.Stages[stage.From.Stage.Index]
		} else {
			stage = nil
		}
	}

	// We need to process labels starting from the base stage through to the final stage
	slices.Reverse(stageChain)

	var baseImage string
	baseStage := stageChain[0]
	if !baseStage.From.Scratch && baseStage.From.Image != nil {
		baseImage = *baseStage.From.Image
	}

	labels := make(map[string]string)
	for _, stage := range stageChain {
		maps.Copy(labels, getStageLabels(stage))
	}

	return baseImage, labels
}

func getStageLabels(stage *dockerfile.Stage) map[string]string {
	if stage == nil {
		return nil
	}

	labels := make(map[string]string)
	for _, cmd := range stage.Commands {
		if labelCmd, ok := cmd.Command.(*instructions.LabelCommand); ok {
			for _, kv := range labelCmd.Labels {
				labels[kv.Key] = kv.Value
			}
		}
	}
	return labels
}

// Determine if we should try to pull the image.
//
// A base image in the Containerfile can use any of the container transports [1].
// We only care about container images from a registry, i.e. those that do not specify
// a transport or use the docker:// transport. The containers-storage: transport is also
// trivially supportable (the image is already present in the local storage).
//
// Most of the others are unsupportable (they can reference a local path, which can get
// created dynamically during the build) or just aren't a real use case.
//
// [1]: https://man.archlinux.org/man/containers-transports.5.en
func isPullableImage(imageRef string) bool {
	if imageRef == "" {
		return false
	}

	transport, _ := splitTransport(imageRef)
	switch transport {
	case "", "docker://", "containers-storage:":
		return true
	default:
		return false
	}
}

// Split an image ref that includes a transport into (transport, image ref).
//
// Example:
// - "docker://registry.io/image:tag" -> ("docker://", "registry.io/image:tag")
// - "registry.io/image:tag" -> ("", "registry.io/image:tag")
func splitTransport(imageRef string) (string, string) {
	transports := []string{
		"docker://",
		"containers-storage:",
		"dir:",
		"docker-archive:",
		"docker-daemon:",
		"oci:",
		"oci-archive:",
		"sif:",
	}
	for _, transport := range transports {
		if imageRef, ok := strings.CutPrefix(imageRef, transport); ok {
			return transport, imageRef
		}
	}
	return "", imageRef
}

func (c *Build) getImageLabels(imageRef string) (map[string]string, error) {
	l.Logger.Debugf("Pulling image %s to read labels...", imageRef)
	err := c.pullImage(imageRef)
	if err != nil {
		return nil, fmt.Errorf("pulling image %s: %w", imageRef, err)
	}

	// buildah inspect doesn't support the <transport>: prefix, strip it
	_, inspectableRef := splitTransport(imageRef)
	info, err := c.CliWrappers.BuildahCli.InspectImage(inspectableRef)
	if err != nil {
		return nil, fmt.Errorf("inspecting image %s: %w", inspectableRef, err)
	}

	return info.OCIv1.Config.Labels, nil
}

// Pull all images referenced by the target stage and its dependencies.
// Primarily needed for hermetic builds where network access is disabled,
// but also useful to ensure image pulls use our retry logic instead of relying on buildah.
// Returns the list of pulled base images.
func (c *Build) prePullBaseImages(df *dockerfile.Dockerfile) ([]string, error) {
	if df == nil || len(df.Stages) == 0 {
		return nil, nil
	}

	var targetStages []int
	if c.Params.Target != "" {
		if stages, ok := findMatchingStages(df.Stages, c.Params.Target); ok {
			if slices.Compare(c.parsedBuildahVersion, []int{1, 44, 0}) >= 0 {
				// Buildah v1.44.0 builds all matching stages
				targetStages = stages
			} else {
				// Earlier buildah versions select the first matching stage
				targetStages = stages[:1]
			}
		} else {
			return nil, fmt.Errorf("target stage %q not found", c.Params.Target)
		}
	} else {
		targetStages = []int{len(df.Stages) - 1}
	}

	var pulledImages []string

	for _, image := range c.collectBaseImages(df, targetStages...) {
		if !isPullableImage(image) {
			l.Logger.Warnf("Skipping pre-pull of %s: unsupported transport", image)
			continue
		}
		l.Logger.Debugf("Pre-pulling base image: %s", image)
		if err := c.pullImage(image); err != nil {
			return nil, fmt.Errorf("pre-pulling image %s: %w", image, err)
		}
		pulledImages = append(pulledImages, image)
	}

	return pulledImages, nil
}

func (c *Build) pullImage(imageRef string) error {
	var extraEnv []string
	// Work around https://github.com/podman-container-tools/buildah/issues/6903.
	// Limit this to buildah v1.44.0; tests will let us know in case later versions need it too.
	if slices.Equal(c.parsedBuildahVersion, []int{1, 44, 0}) {
		extraEnv = append(extraEnv, "_CONTAINERS_USERNS_CONFIGURED=done")
	}

	return c.CliWrappers.BuildahCli.Pull(&cliWrappers.BuildahPullArgs{
		Image:     imageRef,
		HttpProxy: c.Params.ImagePullProxy,
		NoProxy:   c.Params.ImagePullNoProxy,
		TLSVerify: &c.Params.SrcTLSVerify,
		ExtraEnv:  extraEnv,
	})
}

// Verify that each pre-pulled base image has an architecture matching the host
// to prevent emulation builds, which are not allowed.
//
// If the image was a multi-arch index without the correct arch, buildah pull would
// have already failed. This check catches single-arch references, where buildah
// silently pulls the wrong architecture.
func (c *Build) verifyBaseImageArchitectures(images []string) error {
	hostArch := platforms.Normalize(platforms.DefaultSpec()).Architecture

	for _, image := range images {
		_, inspectableRef := splitTransport(image)
		info, err := c.CliWrappers.BuildahCli.InspectImage(inspectableRef)
		if err != nil {
			return fmt.Errorf("inspecting base image %s: %w", image, err)
		}
		if info.OCIv1.Architecture != hostArch {
			return fmt.Errorf(
				"base image %s has architecture '%s', expected '%s'. Use a multi-arch image reference instead of a single-architecture reference",
				image, info.OCIv1.Architecture, hostArch,
			)
		}
	}
	return nil
}

// Collect all images needed to build the target stage(s).
//
// The plural comes from https://github.com/containers/buildah/issues/6731:
// When --target=<name> matches multiple stages with the same name, buildah version 1.44.0
// builds all of them (the last one becomes the build output).
//
// Logic:
// For all the "stages of interest":
//   - For all the instructions that support a 'from' reference:
//     (those being 'FROM <ref>', 'COPY --from=<ref>', 'RUN --mount=from=<ref>'):
//     -- If <ref> is an image, collect this image
//     -- If <ref> is an earlier stage in the containerfile, also collect images for that stage
//
// With skip-unused-stages=true (the default), only the target stages are of interest.
// With skip-unused-stages=false, it's all the stages up to and including the last target stage.
func (c *Build) collectBaseImages(df *dockerfile.Dockerfile, targetStages ...int) []string {
	if len(targetStages) == 0 {
		panic("need at least one target stage")
	}

	baseImageSet := make(map[string]struct{})

	stagesToProcess := []int{}
	stagesSeen := make(map[int]struct{})

	enqueue := func(stageIndexes ...int) {
		for _, stageIdx := range stageIndexes {
			if _, seen := stagesSeen[stageIdx]; !seen {
				stagesSeen[stageIdx] = struct{}{}
				stagesToProcess = append(stagesToProcess, stageIdx)
			}
		}
	}
	if c.Params.SkipUnusedStages {
		enqueue(targetStages...)
	} else {
		// skip-unused-stages=false => buildah builds all stages up to and including the target stage
		// The graph search algorithm is unnecessary in this case, but let's keep things simple
		for i := range slices.Max(targetStages) + 1 {
			enqueue(i)
		}
	}

	for len(stagesToProcess) > 0 {
		stageIdx := stagesToProcess[0]
		stage := df.Stages[stageIdx]
		stagesToProcess = stagesToProcess[1:]

		if stage.From.Image != nil {
			baseImageSet[*stage.From.Image] = struct{}{}
		} else if stage.From.Stage != nil {
			enqueue(stage.From.Stage.Index)
		}

		// 'From' refs can only reference earlier stages. If they reference a later stage
		// (or the same stage in which they appear), buildah treats them as external images.
		// Note: for FROM instructions, dockerfile-json handles this on its own.
		precedingStages := df.Stages[:stageIdx]

		for _, ref := range getFromRefsInCommands(stage) {
			if stages, ok := findMatchingStages(precedingStages, ref); ok {
				// ref matches one or more stages
				// buildah (even before v1.44.0) builds all matching stages, pre-pull all the images
				enqueue(stages...)
			} else {
				// ref is an image
				// (the third option is that ref is a --build-context,
				//  but we don't expose any way to add build contexts)
				baseImageSet[ref] = struct{}{}
			}
		}
	}

	return slices.Sorted(maps.Keys(baseImageSet))
}

// Given a list of containerfile stages and a string ref, determine if the ref matches any stage(s).
// If yes, return ({indexes of matching stages}, true).
//
// First, look for matching named stages ('FROM ... AS name').
// If no such stage exists, the ref may be an integer index of a stage - parse it and verify bounds.
// If ref doesn't match any named stage and isn't a valid integer index, return (nil, false).
func findMatchingStages(stages []*dockerfile.Stage, ref string) ([]int, bool) {
	var matchingStages []int
	for i, stage := range stages {
		if stage.Name != nil && *stage.Name == ref {
			matchingStages = append(matchingStages, i)
		}
	}
	if len(matchingStages) > 0 {
		return matchingStages, true
	}

	if i, err := strconv.Atoi(ref); err == nil && 0 <= i && i < len(stages) {
		return []int{i}, true
	}

	return nil, false
}

// Returns all 'from' references from a stage's commands (COPY --from and RUN --mount=from).
func getFromRefsInCommands(stage *dockerfile.Stage) []string {
	var refs []string
	for _, cmd := range stage.Commands {
		if copyCmd, ok := cmd.Command.(*instructions.CopyCommand); ok && copyCmd.From != "" {
			refs = append(refs, copyCmd.From)
		}
		for _, mount := range cmd.Mounts {
			if mount.From != "" {
				refs = append(refs, mount.From)
			}
		}
	}
	return refs
}

func (c *Build) buildImage() (err error) {
	l.Logger.Info("Building container image...")

	var originalCwd string
	originalCwd, err = os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Chdir(c.effectiveContextDir()); err != nil {
		return fmt.Errorf("couldn't cd to context directory: %w", err)
	}
	defer func() {
		if e := os.Chdir(originalCwd); e != nil && err == nil {
			err = e
		}
	}()

	containerfilePath := c.containerfilePath
	if c.containerfileCopyPath != "" {
		containerfilePath = c.containerfileCopyPath
	}

	buildArgs := &cliWrappers.BuildahBuildArgs{
		Containerfile:    containerfilePath,
		ContextDir:       c.effectiveContextDir(),
		OutputRef:        c.Params.OutputRef,
		Secrets:          c.buildahSecrets,
		Volumes:          c.buildahVolumes,
		BuildArgs:        c.Params.BuildArgs,
		BuildArgsFile:    c.Params.BuildArgsFile,
		Envs:             c.Params.Envs,
		Labels:           c.mergedLabels,
		Annotations:      c.mergedAnnotations,
		SourceDateEpoch:  c.Params.SourceDateEpoch,
		RewriteTimestamp: c.Params.RewriteTimestamp,
		ExtraArgs:        c.Params.ExtraArgs,
		InheritLabels:    &c.Params.InheritLabels,
		Target:           c.Params.Target,
		SkipUnusedStages: &c.Params.SkipUnusedStages,
		TLSVerify:        &c.Params.SrcTLSVerify,
		Squash:           c.Params.Squash,
		OmitHistory:      c.Params.OmitHistory,
		NoCache:          c.Params.NoCache,
		SecurityOpts:     c.Params.SecurityOpts,
		CapAdd:           c.Params.CapAdd,
		CapDrop:          c.Params.CapDrop,
		Devices:          c.Params.Devices,
		Ulimits:          c.Params.Ulimits,
	}
	if c.Params.Hermetic {
		wrapper := cliWrappers.JoinWrappers(
			// We want to build entirely without network access, including ADD instructions.
			// Buildah has a --network=none flag, but it only affects RUN instructions, not ADD.
			// And it doesn't work with BUILDAH_ISOLATION=chroot, the typical setup in Konflux.
			// Instead, we create a network namespace manually using 'unshare --net'.
			c.CliWrappers.Unshare.WithArgs("--net"),
			// But bring up the loopback interface inside this namespace.
			// Mainly needed for Bazel builds, Bazel runs a server on localhost.
			c.CliWrappers.SelfInUserNamespace.WithArgs("--loopback-up"),
		)
		buildArgs.Wrapper = &wrapper
	}
	if c.Params.WorkdirMount != "" {
		buildArgs.Volumes = append(buildArgs.Volumes, cliWrappers.BuildahVolume{
			HostDir: c.effectiveContextDir(), ContainerDir: c.Params.WorkdirMount, Options: "z"})
	}
	if c.buildinfoBuildContext != nil {
		buildArgs.BuildContexts = []cliWrappers.BuildahBuildContext{*c.buildinfoBuildContext}
	}

	if err := buildArgs.MakePathsAbsolute(originalCwd); err != nil {
		return err
	}

	if err := c.CliWrappers.BuildahCli.Build(buildArgs); err != nil {
		return err
	}

	l.Logger.Info("Build completed successfully")
	return nil
}

func (c *Build) pushImage() (string, error) {
	l.Logger.Infof("Pushing image to registry: %s", c.Params.OutputRef)

	pushArgs := &cliWrappers.BuildahPushArgs{
		Image:     c.Params.OutputRef,
		TLSVerify: &c.Params.DestTLSVerify,
	}

	digest, err := c.CliWrappers.BuildahCli.Push(pushArgs)
	if err != nil {
		return "", err
	}

	l.Logger.Info("Push completed successfully")
	l.Logger.Infof("Image digest: %s", digest)

	return digest, nil
}

func (c *Build) writeContainerfileJson(containerfile *dockerfile.Dockerfile, outputPath string) error {
	l.Logger.Infof("Writing parsed Containerfile to: %s", outputPath)

	jsonData, err := json.MarshalIndent(containerfile, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal Containerfile to JSON: %w", err)
	}

	if err := os.WriteFile(outputPath, jsonData, 0644); err != nil {
		return fmt.Errorf("failed to write Containerfile JSON: %w", err)
	}

	l.Logger.Info("Containerfile JSON written successfully")
	return nil
}

// Resolve the pulled images (the input refs from the containerfile) to their canonical forms
// (fully-qualified-name[:tag]@digest) and write the results to the specified path.
//
// The file format is one pair of space-separated "input-ref canonical-ref" per line.
func (c *Build) writeResolvedBaseImages(pulledImages []string, outputPath string) error {
	l.Logger.Infof("Writing resolved base images to: %s", outputPath)

	resolvedImages, err := c.resolveBaseImages(pulledImages)
	if err != nil {
		return fmt.Errorf("determining resolved base images: %w", err)
	}

	var s strings.Builder

	for i := range pulledImages {
		s.WriteString(pulledImages[i])
		s.WriteByte(' ')
		s.WriteString(resolvedImages[i])
		s.WriteByte('\n')
	}

	err = os.WriteFile(outputPath, []byte(s.String()), 0644)
	if err != nil {
		return fmt.Errorf("writing resolved base images: %w", err)
	}
	l.Logger.Info("Resolved base images written successfully")
	return nil
}

func (c *Build) resolveBaseImages(pulledImages []string) ([]string, error) {
	var resolvedImages []string

	for _, image := range pulledImages {
		_, bareImage := splitTransport(image)

		inputRef, err := reference.Parse(bareImage)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", bareImage, err)
		}

		_, hasDigest := inputRef.(reference.Digested)
		if hasDigest && common.IsNormalizedRef(bareImage) {
			l.Logger.Debugf("Resolving base images: input already canonical: %s", bareImage)
			resolvedImages = append(resolvedImages, inputRef.String())
			continue
		}

		entries, err := c.CliWrappers.BuildahCli.ImagesJson(&cliWrappers.BuildahImagesArgs{Image: bareImage})
		if err != nil {
			return nil, fmt.Errorf("buildah images %s: %w", bareImage, err)
		}
		if len(entries) == 0 {
			return nil, fmt.Errorf("'buildah images %s' succeeded but returned 0 entries", bareImage)
		}

		var resolvedRef reference.Named

		// Fully qualified name: take from 'buildah images' output.
		name := entries[0].Names[0]
		if ref, err := reference.Parse(name); err != nil {
			return nil, fmt.Errorf("parsing %s (from 'buildah images %s'): %w", name, bareImage, err)
		} else if namedRef, ok := ref.(reference.Named); !ok {
			return nil, fmt.Errorf("'buildah images %s' returned bogus image name: %s", bareImage, name)
		} else {
			// Drop the tag from the name if any; relying on 'buildah images' for tags is unreliable.
			// The Names array records every reference that has been used to pull the same image,
			// so we may find a tag even if the input ref doesn't have one. Or we may find a different
			// tag than the one in the input ref. This technically doesn't matter, since tags have no
			// authoritative informational value, but it would make the resolution hard to understand.
			resolvedRef = reference.TrimNamed(namedRef)
		}

		// Tag: take from input image or leave empty
		if t, ok := inputRef.(reference.Tagged); ok {
			resolvedRef, err = reference.WithTag(resolvedRef, t.Tag())
			if err != nil {
				panic("invalid tag in valid tagged ref: " + t.String())
			}
		}

		// Digest: take from input image or from 'buildah images' output
		if d, ok := inputRef.(reference.Digested); ok {
			resolvedRef, err = reference.WithDigest(resolvedRef, d.Digest())
			if err != nil {
				panic("invalid digest in valid digested ref: " + d.String())
			}
		} else {
			// This is also a little unpredictable, because if the input ref is a manifest list,
			// then the Digest could be either the manifest list digest or a manifest digest
			// (depending on how this image was pulled the first time it was pulled).
			// But it's the best we can do at this point.
			resolvedRef, err = reference.WithDigest(resolvedRef, digest.Digest(entries[0].Digest))
			if err != nil {
				return nil, fmt.Errorf("'buildah images %s' returned bogus digest: %s", bareImage, entries[0].Digest)
			}
		}

		l.Logger.Debugf("Resolving base images: %s resolved to %s", bareImage, resolvedRef)
		resolvedImages = append(resolvedImages, resolvedRef.String())
	}

	return resolvedImages, nil
}
