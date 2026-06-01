package commands

import (
	"runtime"

	"github.com/konflux-ci/konflux-build-cli/pkg/cliwrappers"
)

var _ cliwrappers.SkopeoCliInterface = &mockSkopeoCli{}

type mockSkopeoCli struct {
	CopyFunc    func(args *cliwrappers.SkopeoCopyArgs) error
	InspectFunc func(args *cliwrappers.SkopeoInspectArgs) (string, error)
}

func (m *mockSkopeoCli) Copy(args *cliwrappers.SkopeoCopyArgs) error {
	if m.CopyFunc != nil {
		return m.CopyFunc(args)
	}
	return nil
}

func (m *mockSkopeoCli) Inspect(args *cliwrappers.SkopeoInspectArgs) (string, error) {
	if m.InspectFunc != nil {
		return m.InspectFunc(args)
	}
	return "", nil
}

var _ cliwrappers.BuildahCliInterface = &mockBuildahCli{}

type mockBuildahCli struct {
	BuildFunc           func(args *cliwrappers.BuildahBuildArgs) error
	PushFunc            func(args *cliwrappers.BuildahPushArgs) (string, error)
	PullFunc            func(args *cliwrappers.BuildahPullArgs) error
	InspectFunc         func(args *cliwrappers.BuildahInspectArgs) (string, error)
	InspectImageFunc    func(name string) (cliwrappers.BuildahImageInfo, error)
	VersionFunc         func() (cliwrappers.BuildahVersionInfo, error)
	ManifestCreateFunc  func(args *cliwrappers.BuildahManifestCreateArgs) error
	ManifestAddFunc     func(args *cliwrappers.BuildahManifestAddArgs) error
	ManifestInspectFunc func(args *cliwrappers.BuildahManifestInspectArgs) (string, error)
	ManifestPushFunc    func(args *cliwrappers.BuildahManifestPushArgs) (string, error)
	ImagesFunc          func(args *cliwrappers.BuildahImagesArgs) (string, error)
	ImagesJsonFunc      func(args *cliwrappers.BuildahImagesArgs) ([]cliwrappers.BuildahImagesEntry, error)
	FromFunc            func(image string) (string, error)
	RmFunc              func(container string) error
	MountFunc           func(container string) (string, error)
}

func (m *mockBuildahCli) Build(args *cliwrappers.BuildahBuildArgs) error {
	if m.BuildFunc != nil {
		return m.BuildFunc(args)
	}
	return nil
}

func (m *mockBuildahCli) Push(args *cliwrappers.BuildahPushArgs) (string, error) {
	if m.PushFunc != nil {
		return m.PushFunc(args)
	}
	return "", nil
}

func (m *mockBuildahCli) Pull(args *cliwrappers.BuildahPullArgs) error {
	if m.PullFunc != nil {
		return m.PullFunc(args)
	}
	return nil
}

func (m *mockBuildahCli) Inspect(args *cliwrappers.BuildahInspectArgs) (string, error) {
	if m.InspectFunc != nil {
		return m.InspectFunc(args)
	}
	return "", nil
}

func (m *mockBuildahCli) InspectImage(name string) (cliwrappers.BuildahImageInfo, error) {
	if m.InspectImageFunc != nil {
		return m.InspectImageFunc(name)
	}
	info := cliwrappers.BuildahImageInfo{}
	info.OCIv1.Architecture = runtime.GOARCH
	return info, nil
}

func (m *mockBuildahCli) Version() (cliwrappers.BuildahVersionInfo, error) {
	if m.VersionFunc != nil {
		return m.VersionFunc()
	}
	return cliwrappers.BuildahVersionInfo{Version: "1.0.0"}, nil
}

func (m *mockBuildahCli) ManifestCreate(args *cliwrappers.BuildahManifestCreateArgs) error {
	if m.ManifestCreateFunc != nil {
		return m.ManifestCreateFunc(args)
	}
	return nil
}

func (m *mockBuildahCli) ManifestAdd(args *cliwrappers.BuildahManifestAddArgs) error {
	if m.ManifestAddFunc != nil {
		return m.ManifestAddFunc(args)
	}
	return nil
}

func (m *mockBuildahCli) ManifestInspect(args *cliwrappers.BuildahManifestInspectArgs) (string, error) {
	if m.ManifestInspectFunc != nil {
		return m.ManifestInspectFunc(args)
	}
	return "", nil
}

func (m *mockBuildahCli) ManifestPush(args *cliwrappers.BuildahManifestPushArgs) (string, error) {
	if m.ManifestPushFunc != nil {
		return m.ManifestPushFunc(args)
	}
	return "", nil
}

func (m *mockBuildahCli) Images(args *cliwrappers.BuildahImagesArgs) (string, error) {
	if m.ImagesFunc != nil {
		return m.ImagesFunc(args)
	}
	return "", nil
}

func (m *mockBuildahCli) ImagesJson(args *cliwrappers.BuildahImagesArgs) ([]cliwrappers.BuildahImagesEntry, error) {
	if m.ImagesJsonFunc != nil {
		return m.ImagesJsonFunc(args)
	}
	return nil, nil
}

func (m *mockBuildahCli) From(image string) (string, error) {
	if m.FromFunc != nil {
		return m.FromFunc(image)
	}
	return "", nil
}

func (m *mockBuildahCli) Rm(container string) error {
	if m.RmFunc != nil {
		return m.RmFunc(container)
	}
	return nil
}

func (m *mockBuildahCli) Mount(container string) (string, error) {
	if m.MountFunc != nil {
		return m.MountFunc(container)
	}
	return "", nil
}

var _ cliwrappers.SubscriptionManagerCliInterface = &mockSubscriptionManagerCli{}

type mockSubscriptionManagerCli struct {
	RegisterFunc   func(params *cliwrappers.SubscriptionManagerRegisterParams) error
	UnregisterFunc func()
}

func (m *mockSubscriptionManagerCli) Register(params *cliwrappers.SubscriptionManagerRegisterParams) error {
	if m.RegisterFunc != nil {
		return m.RegisterFunc(params)
	}
	return nil
}

func (m *mockSubscriptionManagerCli) Unregister() {
	if m.UnregisterFunc != nil {
		m.UnregisterFunc()
	}
}

var _ cliwrappers.SyftCliInterface = &mockSyftCli{}

type mockSyftCli struct {
	ScanFunc func(args *cliwrappers.SyftScanArgs) (string, error)
}

func (m *mockSyftCli) Scan(args *cliwrappers.SyftScanArgs) (string, error) {
	if m.ScanFunc != nil {
		return m.ScanFunc(args)
	}
	return "", nil
}

var _ cliwrappers.OrasCliInterface = &mockOrasCli{}

type mockOrasCli struct {
	Executor cliwrappers.CliExecutorInterface
	PushFunc func(args *cliwrappers.OrasPushArgs) (string, string, error)
}

func (m *mockOrasCli) Push(args *cliwrappers.OrasPushArgs) (string, string, error) {
	if m.PushFunc != nil {
		return m.PushFunc(args)
	}
	return "", "", nil
}
