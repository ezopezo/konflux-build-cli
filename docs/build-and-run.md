# Building and running the CLI

## How to build

```sh
go build -tags exclude_graphdriver_btrfs -o konflux-build-cli main.go
```
or statically:
```sh
CGO_ENABLED=0 go build -tags exclude_graphdriver_btrfs -o konflux-build-cli main.go
```
or in debug mode:
```sh
go build -tags exclude_graphdriver_btrfs -gcflags "all=-N -l" -o konflux-build-cli main.go
```

## How to run / debug a command on host

Build the CLI and setup the command environment.

Parameters can be passed via CLI arguments or environment variables, CLI arguments take precedence.
For example:

```sh
./konflux-build-cli my-command --image-url quay.io/namespace/image:tag --digest sha256:abcde1234 --tags tag1 tag2 --result-sha=/tmp/my-command-result-sha
```

Alternatively, it's possible to provide data via environment variables:

```sh
# my-command-env.sh
export KBC_MYCOMMAND_IMAGE_URL=quay.io/namespace/image:tag
export KBC_MYCOMMAND_DIGEST=sha256:abcde1234
export KBC_MYCOMMAND_TAGS='tag1 tag2'
export KBC_MYCOMMAND_SOME_FLAG=true

export KBC_MYCOMMAND_RESULTS_DIR="/tmp/my-command-results"
mkdir -p "$RESULTS_DIR"
export KBC_MYCOMMAND_RESULT_SHA="${RESULTS_DIR}/RESULT_SHA"
```
or store the above in an `*.sh` file and source it:
```sh
. my-command-env.sh
./konflux-build-cli my-command
```
or mix approaches:
```sh
export KBC_MYCOMMAND_RESULT_FILE_SHA=/tmp/my-command-result-sha
./konflux-build-cli my-command --image-url quay.io/namespace/image:tag --digest sha256:abcde1234 --tags tag1 tag2
```

## How to run / debug a command in container

It's possible to use both `docker` or `podman`.

Build Konflux Build CLi image:
```sh
docker build -f Dockerfile -t konflux-build-cli .
```

Run a command:
```sh
docker run --rm -it -e SOME_VAR=val quay.io/konflux-ci/task-runner:latest my-command --arg=val --flag
```

## References

See [command architecture principles and design](design/command.md) for more details.
