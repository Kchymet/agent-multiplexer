package licenses

import (
	"debug/buildinfo"
	"runtime/debug"
	"strings"
	"testing"
)

func TestVerifyBuildInfoAcceptsMatchingArtifact(t *testing.T) {
	info := fixtureBuildInfo("go1.26.10")
	if err := VerifyBuildInfo(info, fixtureBundle()); err != nil {
		t.Fatalf("VerifyBuildInfo: %v", err)
	}
}

func TestVerifyBuildInfoRejectsToolchainMismatch(t *testing.T) {
	info := fixtureBuildInfo("go1.27.3")
	err := VerifyBuildInfo(info, fixtureBundle())
	if err == nil || !strings.Contains(err.Error(), "toolchain") {
		t.Fatalf("error = %v, want toolchain mismatch", err)
	}
}

func fixtureBuildInfo(goVersion string) *buildinfo.BuildInfo {
	return &buildinfo.BuildInfo{
		GoVersion: goVersion,
		Deps: []*debug.Module{
			{Path: "example.test/dependency", Version: "v1.2.3", Sum: "h1:fixture="},
			{Path: "github.com/kchymet/agent-multiplexer/harnessproto", Version: "v0.0.0", Replace: &debug.Module{Path: "./harnessproto", Version: "(devel)"}},
		},
		Settings: []debug.BuildSetting{
			{Key: "GOOS", Value: "linux"},
			{Key: "GOARCH", Value: "amd64"},
			{Key: "CGO_ENABLED", Value: "0"},
		},
	}
}

func fixtureBundle() string {
	return "Supported targets: darwin/amd64, darwin/arm64, linux/amd64, linux/arm64\n" +
		"Go toolchain: go1.26.10\n" +
		"First-party module paths excluded: github.com/kchymet/agent-multiplexer/harnessproto\n" +
		"example.test/dependency@v1.2.3\n" +
		"  module checksum: h1:fixture=\n"
}
