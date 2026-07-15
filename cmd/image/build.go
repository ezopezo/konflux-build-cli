package image

import (
	"github.com/spf13/cobra"

	"github.com/konflux-ci/konflux-build-cli/pkg/commands"
	"github.com/konflux-ci/konflux-build-cli/pkg/common"
	l "github.com/konflux-ci/konflux-build-cli/pkg/logger"
)

var BuildCmd = &cobra.Command{
	Use:   "build",
	Short: "Build a container image",
	Long: `Build a container image using buildah.

Optionally, push the built image to a registry using the --push flag.

The command outputs the image URL and optionally the image digest (if pushing).

Secret Handling:
  Use --secret-dirs to provide directories containing secret files that should
  be available during the build. Each file in the root of a secret directory is
  added as a 'buildah build --secret' argument.

  Accepts the following forms of arguments:
    src=DIR_PATH[,name=BASENAME][,optional=true|false]
        Makes the files from DIR_PATH available with id=<BASENAME>/<filename>.
        The BASENAME defaults to the basename of DIR_PATH.
        If optional=true and DIR_PATH doesn't exist, it is skipped.

    DIR_PATH
        Equivalent to src=DIR_PATH

  Access the secrets with 'RUN --mount=type=secret,id=<basename>/<filename>'.
  The --mount option makes them available at /run/secrets/<basename>/<filename>
  for that particular RUN instruction.

Red Hat Subscription Management (RHSM) Handling:
  Fedora and RHEL machines typically have implicit RHSM integration, where if
  the host is subscribed, containers automatically get the subscription as well.
  Konflux-build-cli disables this and provides explicit options instead.

  1) Entitlement certificates (--rhsm-entitlements=DIRECTORY)

    If you already have entitlement certificates, e.g. because your system
    is already subscribed, use --rhsm-entitlements=/etc/pki/entitlement
    (or another directory). This mounts the certificates at /etc/pki/entitlement
    during the build, allowing dnf/microdnf to access entitled content.

    Note that this approach may not be suitable for use in CI pipelines,
    because the subscription server regularly rotates entitlement certificates.
    If you store them long-term as CI secrets, they may become invalid.

  2) Activation keys (--rhsm-activation-key=FILE + --rhsm-org=FILE)

    Get an RHSM activation key and organization ID and store them as files.
    Use these to activate the subscription yourself in the containerfile or to
    have konflux-build-cli activate it for you.

    a) Self-registration (--rhsm-activation-mount=DEST-DIR)

      The activation key will be available at DEST-DIR/activationkey,
      the org ID at DEST-DIR/org. Use these to activate the subscription
      with:

        subscription-manager register \
          --activationkey="$(cat DEST-DIR/activationkey)" \
          --org="$(cat DEST-DIR/org)"

      Don't forget to 'subscription-manager unregister' after installing the
      required RPMs, to avoid leaving a dangling registration on the server.

    b) Pre-registration (--rhsm-activation-preregister)

      Konflux-build-cli runs 'subscription-manager register' using the provided
      activation key and org ID, and mounts the resulting certificates into the
      build.

      Requires root permissions.

      WARNING: this first unregisters your host system if already registered.
      Konflux-build-cli also unregisters itself after the build completes,
      leaving the host system unregistered regardless of its original state.
      This may be less of a problem in CI pipelines (but do note the requirement
      to run as root).

    The activation keys approach is more suitable for CI pipelines, because
    unlike entitlement certificates, activation keys do not expire.

  RHSM CA certificates
    To access entitled content, dnf also needs the CA certificates for the
    servers that provide said content. The certificates come with the
    subscription-manager packages.

    To better support builds where subscription-manager is not installed in the
    base image, konflux-build-cli mounts the CA certificates from the host into
    the build if necessary. More specifically, the CLI mounts the certificates
    in all cases except when using activation keys without pre-registration
    (because the build must have subscription-manager installed in this case).
    If the certificates don't exist on the host, the CLI logs a warning and
    proceeds. The build may still work if the base image already has the
    certificates or if the containerfile installs them.

    Customize this behavior with --rhsm-mount-ca-certs={auto|always|never}:
      'auto' is the behavior described above (the default)
      'always' always mounts the certs, failing if they don't exist on the host
      'never' never mounts the certs

Examples:
  # Build using auto-detected Containerfile/Dockerfile in current directory
  konflux-build-cli image build -t quay.io/myorg/myimage:latest

  # Build and push to registry
  konflux-build-cli image build -t quay.io/myorg/myimage:latest --push

  # Build and push with additional tags (e.g., version, commit SHA)
  konflux-build-cli image build -t quay.io/myorg/myimage:latest \
    --additional-tags v1.0.0 commit-abc123 --push

  # Build with explicit Containerfile and context
  konflux-build-cli image build -f ./Containerfile -c ./myapp -t quay.io/myorg/myimage:v1.0.0

  # Build with secrets from directories
  konflux-build-cli image build -t quay.io/myorg/myimage:latest \
    --secret-dirs /path/to/secrets1 src=/path/to/secrets2,name=certs

  # Build with additional buildah arguments
  konflux-build-cli image build -t quay.io/myorg/myimage:latest -- --compat-volumes --force-rm
`,
	Run: func(cmd *cobra.Command, args []string) {
		l.Logger.Debug("Starting build")
		build, err := commands.NewBuild(cmd, args)
		if err != nil {
			l.Logger.Fatal(err)
		}
		if err := build.Run(); err != nil {
			l.Logger.Fatal(err)
		}
		l.Logger.Debug("Finished build")
	},
}

func init() {
	common.RegisterParameters(BuildCmd, commands.BuildParamsConfig)
}
