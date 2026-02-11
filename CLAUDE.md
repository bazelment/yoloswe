This is a Bazel-managed monorepo with a collection of multiple Go projects.

## Build and Test
Always use `bazel build //...` and `bazel test //...` to build and test the project.
Never use `go build` or `go test` directly.

Always run lint/type-check/tests before creating or pushing to a PR. Never create a PR without passing quality gates first.
Run `scripts/lint.sh` before running `bazel test //...` to catch lint issues early.

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
- When running `bazel test`, always start with a small timeout like 1 minute and only increase when necessary.
- When debugging test that couldn't finish properly and ends up being interrupted or timeout, launch the test binary directly under `bazel-bin/` as subprocess, so the terminal will show the progress and it's more clear where it's stuck.
    * Also consider using framework specific test case filter to only run failing test to speed up iteration process.
    * Tune framework specific test log verbosity to better understand the test code behaviour.
    * If you see the need to run one off test case to better understand some code's behaviour, this indicates a test coverage gap, put the as real test code into the code base you are working on.
- All test cases should be ready to run in parallel, so they should avoid things like writing to the same temp file, using the same port, etc.
- Never use static port in test, pick a random port to avoid port collision.
- The tests should be deterministic, try to avoid external dependencies that has unstable output. When performing validation, avoid conditional check.
- Tests for different purpose like integration should be put into different directory so that gazelle won't blend them into a single build target.

## Integration Tests and Gazelle

Integration test `BUILD.bazel` files are hand-maintained and must include `# gazelle:ignore` as the first line. Gazelle cannot generate these correctly because:
1. Test files behind `//go:build integration` build tags are invisible to Gazelle
2. Gazelle cannot add the required `tags = ["manual", "local"]` or `gotags = ["integration"]` attributes

When creating a new `integration/` test directory:
1. Write the `BUILD.bazel` by hand with `# gazelle:ignore` at the top
2. Include `gotags = ["integration"]` so Bazel passes the build tag to the Go compiler
3. Include `tags = ["manual"]` (and optionally `"local"`) so tests are excluded from `bazel test //...`
4. After running `bazel run //:gazelle`, verify the integration BUILD files were not modified
