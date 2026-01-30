#!/usr/bin/env bash
set -euo pipefail

# Tidy all Go modules in the workspace

# Find workspace root (where go.work is located)
if [[ -n "${BUILD_WORKSPACE_DIRECTORY:-}" ]]; then
    # Running under Bazel
    WORKSPACE_ROOT="${BUILD_WORKSPACE_DIRECTORY}"
else
    # Running directly
    WORKSPACE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fi

cd "${WORKSPACE_ROOT}"

echo "Tidying Go modules in ${WORKSPACE_ROOT}..."

# Parse go.work to find all modules
modules=()
in_use_block=false

while IFS= read -r line; do
    # Check if we're entering the use block
    if [[ "$line" =~ ^use[[:space:]]*\( ]]; then
        in_use_block=true
        continue
    fi
    
    # Check if we're exiting the use block
    if [[ "$in_use_block" == true ]] && [[ "$line" =~ ^\) ]]; then
        in_use_block=false
        continue
    fi
    
    # If we're in the use block, extract module paths
    if [[ "$in_use_block" == true ]]; then
        # Remove leading/trailing whitespace and quotes
        module=$(echo "$line" | sed 's/^[[:space:]]*//' | sed 's/[[:space:]]*$//' | sed 's/"//g')
        if [[ -n "$module" ]]; then
            modules+=("$module")
        fi
    fi
done < go.work

# Run go mod tidy on each module
for module in "${modules[@]}"; do
    if [[ -d "$module" ]] && [[ -f "$module/go.mod" ]]; then
        echo "  Running go mod tidy in $module"
        (cd "$module" && go mod tidy)
    fi
done

echo "Done!"

