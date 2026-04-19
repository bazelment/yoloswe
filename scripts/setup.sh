#!/usr/bin/env bash
# Dev environment setup. Idempotent — safe to re-run.
#
# Installs gstack skills by cloning the pinned version to a shared location
# (~/.gstack/builds/<commit>/) and symlinking into this worktree. Build
# artifacts are shared across all worktrees, so only the first run is slow.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VERSION_FILE="$REPO_ROOT/.gstack-version"

# Skip if this branch doesn't have gstack
if [ ! -f "$VERSION_FILE" ]; then
  exit 0
fi

COMMIT="$(head -1 "$VERSION_FILE" | tr -d '[:space:]')"
if [ -z "$COMMIT" ]; then
  echo "Error: .gstack-version is empty" >&2
  exit 1
fi

GSTACK_HOME="$HOME/.gstack"
GSTACK_REPO="$GSTACK_HOME/repo"
GSTACK_BUILD="$GSTACK_HOME/builds/$COMMIT"
SKILLS_DIR="$REPO_ROOT/.claude/skills"
GSTACK_LINK="$SKILLS_DIR/gstack"

# 1. Clone or fetch the gstack repo
if [ ! -d "$GSTACK_REPO/.git" ]; then
  echo "Cloning gstack..."
  mkdir -p "$GSTACK_HOME"
  git clone --quiet https://github.com/garrytan/gstack.git "$GSTACK_REPO"
else
  # Fetch only if the commit isn't already available
  if ! git -C "$GSTACK_REPO" cat-file -e "$COMMIT" 2>/dev/null; then
    echo "Fetching gstack updates..."
    git -C "$GSTACK_REPO" fetch --quiet origin
  fi
fi

# 2. Create a build directory for this commit if it doesn't exist
if [ ! -d "$GSTACK_BUILD" ]; then
  echo "Preparing gstack $COMMIT..."
  git -C "$GSTACK_REPO" worktree add --quiet --detach "$GSTACK_BUILD" "$COMMIT"
fi

# 3. Build if needed (gstack's setup script has smart rebuild logic)
if [ ! -x "$GSTACK_BUILD/browse/dist/browse" ]; then
  echo "Building gstack (first time, ~30s)..."
  "$GSTACK_BUILD/setup"
fi

# 4. Symlink .claude/skills/gstack → shared build
mkdir -p "$SKILLS_DIR"
ln -snf "$GSTACK_BUILD" "$GSTACK_LINK"

# 5. Create skill symlinks (e.g., .claude/skills/review → gstack/review)
#    gstack's setup only creates these when run from inside .claude/skills/,
#    so we do it ourselves for the shared-build layout.
for skill_dir in "$GSTACK_BUILD"/*/; do
  if [ -f "$skill_dir/SKILL.md" ]; then
    skill_name="$(basename "$skill_dir")"
    [ "$skill_name" = "node_modules" ] && continue
    target="$SKILLS_DIR/$skill_name"
    if [ -L "$target" ] || [ ! -e "$target" ]; then
      ln -snf "gstack/$skill_name" "$target"
    fi
  fi
done

echo "gstack ready."
