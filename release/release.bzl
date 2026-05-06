"""Release packaging helpers."""

load("@rules_go//go:def.bzl", "go_cross_binary")

_RELEASE_PLATFORMS = [
    ("darwin_amd64", "@rules_go//go/toolchain:darwin_amd64"),
    ("darwin_arm64", "@rules_go//go/toolchain:darwin_arm64"),
    ("linux_amd64", "@rules_go//go/toolchain:linux_amd64"),
    ("linux_arm64", "@rules_go//go/toolchain:linux_arm64"),
]

def release_go_binaries(name, target):
    for platform_name, platform in _RELEASE_PLATFORMS:
        go_cross_binary(
            name = "%s_%s" % (name, platform_name),
            platform = platform,
            target = target,
        )
