Integration tests are located under `integration_tests` directory.
Check [integration tests](/docs/design/integration-tests.md) doc for the integration tests structure and design.

# How to run integration tests

## Prerequisites to run integration tests

- `golang` should be installed.
- `docker` or `podman` installed.
- In case of using `docker`, on some Linux systems, one might need to increase open files and watchers limit within container.
  Open `/etc/sysctl.conf` for edit and add / edit the line:
  `fs.inotify.max_user_instances=1024`.
  Then, apply changes by `sudo sysctl -p` or reboot.

See [integration tests settings](#integration-tests-settings) for more details

## How to run integration tests

Integration tests are located under `integration_tests` directory.

To run specific test (test function) from terminal execute:
```sh
go test -tags exclude_graphdriver_btrfs -timeout 5m -run ^TestMyCommand$ ./integration_tests
```
or if the test function has nested `t.Run`s:
```sh
go test -tags exclude_graphdriver_btrfs -timeout 5m -run ^TestBuild$/^UsesRunInstruction$ ./integration_tests
go test -tags exclude_graphdriver_btrfs -timeout 5m -run ^TestBuild$/^UsesRunInstruction$/^AsRoot$ ./integration_tests
```
or use your IDE to run or debug one.

To run all integration tests execute:
```sh
go test -tags exclude_graphdriver_btrfs -timeout 20m ./integration_tests
```

If an IDE is used to run inetgration tests, make sure to configure tests timeout.
For example, in case of VSCode, go to setting and change `Go: Test Timeout`
or create / modify config file `.vscode/settings.json`:
```json
{
    "go.testTimeout": "600s"
}
```

Note, you need to set big timeout in case of just running a test but debugging the CLI inside container.
In such situation better to debug both test (to avoid timeouts) and the CLI itself.

If golang caches the test results with a message like:
```
ok  	github.com/konflux-ci/konflux-build-cli/integration_tests	(cached)
```
and it's needed to rerun the tests anyway, add `-count=1` argument to the test command:
```sh
go test -tags exclude_graphdriver_btrfs -count=1 ./integration_tests
```

## Integration tests settings

There is `integration_tests/framework/settings.go` that holds global integration tests settings.

Available integration tests settings:
 - `Debug`: whether to run the CLI within container in debug mode.
   Useful to troubleshoot single test.
   Note, when debug mode is activated, the CLI won't run until a debugger connects to it (port `2345`).
   To use the debug mode, [Delve](https://github.com/go-delve/delve/tree/master/Documentation/installation) should be installed.
   The `dlv` binary should be in `$GOPATH/bin/` or `~/go/bin/` if `GOPATH` environment variable is not defined.
   Example debug configuration for VSCode:
   ```json
    {
        "name": "Connect into container",
        "type": "go",
        "request": "attach",
        "mode": "remote",
        "port": 2345,
        "host": "127.0.0.1",
        "showLog": true
    }
   ```
 - `LocalRegistry`: whether to use local containerized registry or quay.io.
   See [image registry for integration tests](#image-registry-for-integration-tests) section for more details.

Also, there are the following environment variables:
- `KBC_TEST_CONTAINER_TOOL` defines which container engine to use if both `docker` and `podman` installed.
- `ZOT_REGISTRY_PORT` changes the port Zot registry is run on.
  Note, after changing the port, it's required to edit or regenerate `zot-config.json`.

## Image registry for integration tests

It should be possible to run integration tests using any OCI compatible registry.

Currently, the following registries are supported:
- local [Zot](https://zotregistry.dev) registry running in a container
- [quay.io](https://quay.io/)

Whatever registry is used for tests, the actual implementation is encapsulated by `ImageRegistry` interface.
To change the registry, use `LocalRegistry` [test option](#integration-tests-settings).

### Using local Zot registry for integration tests

Using local Zot registry doesn't require any manual configuration.
The test framework will do everything automatically.
However, in order to do automatic registry configuration, the following tools have to be available in the system:
- `htpasswd`
- `openssl`

The configuration data is saved under `zotdata` directory within `integration_tests` directory.
Typically, `zotdata` contains:
- `ca.crt` self signed root certificate that should be added as trusted to other tools using the registry.
- `ca.key` generated private key used to create the root certificate.
- `server.crt` descendent certificate used in Zot server.
- `server.key` private key used by Zot server to secure connections.
- `zot-config.json` Zot registry configuration file.
- `htpasswd` auth file for Zot registry user.
- `config.json` docker config json that has credentials to push into Zot registry.
  Use `DOCKER_CONFIG=./integration_tests/zotdata/ docker push/pull localhost:5000/my-image` to access the registry.

In case of using `podman`, during the automatic Zot registry configuration,
the test framework will copy the generated self-signed CA certificate into `podman`'s config directory:
`~/.config/containers/certs.d/` under `localhost:5000` folder.

### Using quay.io for integration tests

To use `quay.io` as registry for test, provide the following environments variables:
- `QUAY_NAMESPACE`, for example `username` or `my-org`
- `QUAY_ROBOT_NAME`
- `QUAY_ROBOT_TOKEN`

Also, image repository for tests should be created before the tests run and **be public**.
Note, one can start a test which will create the image repository (private by default) and fail (unless run in debug mode),
then the user needs to switch only the visibility in the image repository settings.

## Logger output and coloring

When running integration tests, logs may show raw ANSI escape codes (e.g., `\x1b[36m`) and escaped
quotes, making the output hard to read. The logger respects the standard
[CLICOLOR](https://bixense.com/clicolors/) environment variables:

- **`CLICOLOR=0`** — disable colored output.
- **`CLICOLOR_FORCE=1`** — force colored output.

## MacOS test specifics

On MacOS, the `/tmp` directory is a symbolic link to the `/private/tmp` directory.
Some unit tests and integration tests rely on verbatim path comparisons.
To avoid unexpected failures, you can set the `TMPDIR` environment variable.
For example:
```sh
mkdir .tmpdir
TMPDIR="$(pwd)/.tmpdir" go test -tags exclude_graphdriver_btrfs ./...
```

## References

See also [integration tests design](design/integration-tests.md).
