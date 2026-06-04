# Build the Konflux Build CLI binary.
# For more details and updates, refer to
# https://catalog.redhat.com/en/software/containers/rhel10/go-toolset/6707d40f27f63a06f78743c4
FROM registry.access.redhat.com/ubi10/go-toolset:1.25@sha256:182645783ad0a0af4a78d928f2d9167815d59c12cc156aa3c229cf3a49d636d9 AS builder
ARG TARGETOS
ARG TARGETARCH

USER 1001

WORKDIR /workspace
# Copy the Go Modules manifests
COPY --chown=1001:0 go.mod go.mod
COPY --chown=1001:0 go.sum go.sum
# Cache deps before building and copying source, so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY --chown=1001:0 . .

# Build
# The GOARCH does not have a default value to allow the binary to be built according to the host where the command was called.
# For example, if we call make docker-build in a local env which has Apple Silicon,
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o konflux-build-cli main.go

# Use the Konflux task-runner image as base for the Konflux Build CLI.
# For more details and updates, refer to https://quay.io/konflux-ci/task-runner
FROM quay.io/konflux-ci/task-runner:1.8.1@sha256:b9ef0479b57a494368a12a02886c650314c9ac7391c6228f21065865aab71c9e
COPY --from=builder /workspace/konflux-build-cli /usr/local/bin/konflux-build-cli
USER 65532:65532

# Required for ecosystem-cert-preflight-checks
# https://access.redhat.com/documentation/en-us/red_hat_software_certification/2024/html-single/red_hat_openshift_software_certification_policy_guide/index#assembly-requirements-for-container-images_openshift-sw-cert-policy-introduction
COPY LICENSE /licenses/LICENSE

LABEL description="Konflux Build CLI"
LABEL io.k8s.description="Konflux Build CLI"
LABEL io.k8s.display-name="konflux-build-cli"
LABEL io.openshift.tags="konflux, build, cli"
LABEL summary="Konflux Build CLI"
LABEL name="konflux-build-cli"
LABEL com.redhat.component="konflux-build-cli"

ENTRYPOINT ["/usr/local/bin/konflux-build-cli"]
