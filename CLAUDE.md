This is a Bazel-managed monorepo with a collection of multiple Go projects.

## Build and Test
Always use `bazel build //...` and `bazel test //...` to build and test the project.
Never use `go build` or `go test` directly.

## What to do when a new Go module is imported

To add a new dependency, please always follow these steps, don't use `go` commands directly:
1. Import the dependency in your go code.
2. Use `bazel run @rules_go//go:go -- mod tidy` inside the directory of the go module to add the dependency to `go.mod` and `go.sum`.
    * Check for version conflicts or ambiguity and add replace directives in `go.work` if needed
    * Repeat that until the `mod tidy` can succeed.
3. Run `bazel run //:tidy` to update the `go.mod` and `go.sum` files across the workspace.
4. Run `bazel run //:gazelle` to update the `BUILD.bazel` files.
5. **Never manually edit BUILD.bazel or go.mod** - always use the proper Bazel commands

## Code Style
- This is a monorepo, breaking API change is OK. API cleanness is more important.

## Test Style

- Avoid using sleep to synchronize in tests, always use proper condition to wait on (with condvar or channel), also consider using `require.Eventually` when it's relevant. Make sure sensible timeout is chosen so the test won't get stuck when it fails to meet certain condition.
- When debugging test that couldn't finish properly and ends up being interrupted or timeout, launch the test binary directly under `bazel-bin/` as subprocess, so the terminal will show the progress and it's more clear where it's stuck.
- All test cases should be ready to run in parallel, so they should avoid things like writing to the same temp file, using the same port, etc.
- Never use static port in test, pick a random port to avoid port collision.
- The tests should be deterministic, try to avoid external dependencies that has unstable output. When performing validation, avoid conditional check.