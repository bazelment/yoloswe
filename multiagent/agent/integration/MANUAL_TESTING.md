# Manual Integration Testing for PlanOnlyPermissionHandler

This directory contains integration tests that verify the `PlanOnlyPermissionHandler` works correctly with a real Gemini CLI process.

## Why Manual Testing?

These tests are marked with `//go:build integration` and are **NOT** run in CI because:

1. They require the Gemini CLI to be installed and configured
2. They require valid API credentials
3. They require the Gemini folder trust setup to be complete
4. They make real API calls (cost money, subject to rate limits)

## Prerequisites

Before running these tests locally, ensure:

1. **Gemini CLI installed**: `gemini` binary is in your PATH
2. **API Key configured**: Valid Gemini API key is set up
3. **Folder trust configured**:
   ```bash
   cd /path/to/this/repo
   gemini  # Run once and select "Trust folder" when prompted
   ```

## Running the Tests

### Option 1: Run all PlanOnlyPermissionHandler tests

```bash
# Build the test binary
bazel build //multiagent/agent/integration:integration_test

# Run the plan permission tests
./bazel-bin/multiagent/agent/integration/integration_test_/integration_test \
  -test.v \
  -test.run TestPlanOnlyPermissionHandler
```

### Option 2: Run a specific test

```bash
# Test that read operations are allowed
./bazel-bin/multiagent/agent/integration/integration_test_/integration_test \
  -test.v \
  -test.run TestPlanOnlyPermissionHandler_ReadOperationsAllowed

# Test that write operations are rejected
./bazel-bin/multiagent/agent/integration/integration_test_/integration_test \
  -test.v \
  -test.run TestPlanOnlyPermissionHandler_WriteOperationsRejected

# Test multi-turn long-running session
./bazel-bin/multiagent/agent/integration/integration_test_/integration_test \
  -test.v \
  -test.run TestPlanOnlyPermissionHandler_LongRunningSession
```

### Option 3: Use Bazel test with manual tag

```bash
# Run integration tests directly with Bazel
# (Note: This requires gemini CLI to be available)
bazel test //multiagent/agent/integration:integration_test \
  --test_filter="TestPlanOnlyPermissionHandler*" \
  --test_output=all \
  --test_timeout=600
```

## What These Tests Verify

### 1. `TestPlanOnlyPermissionHandler_ReadOperationsAllowed`

- Creates a test file with content
- Asks Gemini to read the file
- Verifies that read operations (read_file, read_text_file) are **allowed**
- Verifies the response contains the file content

**Expected result:** Read operations succeed, file content is returned

### 2. `TestPlanOnlyPermissionHandler_WriteOperationsRejected`

- Asks Gemini to write content to a file
- Verifies that write operations (write_file, write_text_file) are **rejected**
- Verifies the file was NOT created on disk

**Expected result:** Write operations are rejected, no file is created

### 3. `TestPlanOnlyPermissionHandler_LongRunningSession`

- Tests a multi-turn session with mixed read/write operations:
  - Turn 1: Read a file (should be allowed)
  - Turn 2: Write to a file (should be rejected)
  - Turn 3: List directory (should be allowed)
- Verifies permissions are enforced consistently across all turns

**Expected result:**
- Read operations succeed in all turns
- Write operations are rejected in all turns
- Session state is maintained correctly

## Debugging

If tests fail, check:

1. **Gemini CLI is working:**
   ```bash
   gemini --version
   echo "Say hello" | gemini
   ```

2. **Folder trust is set up:**
   ```bash
   # Check trust status
   cat ~/.gemini/trustedFolders.json
   # Should contain the path to this repo
   ```

3. **API key is valid:**
   ```bash
   # Gemini should respond successfully
   echo "What is 2+2?" | gemini
   ```

4. **Enable verbose output:**
   ```bash
   # Add -test.v for verbose output
   ./bazel-bin/multiagent/agent/integration/integration_test_/integration_test \
     -test.v -test.run TestPlanOnlyPermissionHandler
   ```

## Test Output Example

Successful test output should look like:

```
=== RUN   TestPlanOnlyPermissionHandler_ReadOperationsAllowed
    plan_permission_test.go:123: Allowed tools: [read_text_file]
    plan_permission_test.go:124: Rejected tools: []
--- PASS: TestPlanOnlyPermissionHandler_ReadOperationsAllowed (5.23s)
=== RUN   TestPlanOnlyPermissionHandler_WriteOperationsRejected
    plan_permission_test.go:175: Rejected tools: [write_text_file]
--- PASS: TestPlanOnlyPermissionHandler_WriteOperationsRejected (3.45s)
```

## Cost Considerations

Each test makes 1-3 API calls to Gemini. At current pricing:
- `gemini-2.5-flash` is very cheap (~$0.001-0.01 per test)
- Running all 3 tests costs approximately **$0.01-0.03**

To minimize costs during development:
- Run tests selectively using `-test.run` filter
- Use the fastest model (`gemini-2.5-flash`)
- Don't run in CI pipelines
