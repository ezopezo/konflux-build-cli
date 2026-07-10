# How to run unit tests

To run all unit tests:
```sh
go test -tags exclude_graphdriver_btrfs ./pkg/...
```

To run unit tests for a package:
```sh
go test -tags exclude_graphdriver_btrfs ./pkg/commands
```

To run specific test from terminal execute:
```sh
go test -tags exclude_graphdriver_btrfs -run ^TestMyCommand_SuccessScenario$ ./pkg/...
```

For a developer, to run or debug a specific test or run all tests in a single file, it's most convenient to use UI of your IDE.

## MacOS test specifics

On MacOS, the `/tmp` directory is a symbolic link to the `/private/tmp` directory.
Some unit tests and integration tests rely on verbatim path comparisons.
To avoid unexpected failures, you can set the `TMPDIR` environment variable.
For example:
```sh
mkdir .tmpdir
TMPDIR="$(pwd)/.tmpdir" go test -tags exclude_graphdriver_btrfs ./...
```
