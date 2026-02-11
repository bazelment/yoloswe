#!/usr/bin/env bash
set -euo pipefail

# Install script for bramble and wt CLI tools
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/bazelment/yoloswe/main/scripts/install.sh | bash
#   curl -fsSL ... | bash -s -- --tool bramble
#   curl -fsSL ... | bash -s -- --tool wt --version v1.2.3 --dir /usr/local/bin

REPO="bazelment/yoloswe"
INSTALL_DIR="${HOME}/.local/bin"
TOOL=""
VERSION=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tool|-t)
      TOOL="$2"
      shift 2
      ;;
    --version|-v)
      VERSION="$2"
      shift 2
      ;;
    --dir|-d)
      INSTALL_DIR="$2"
      shift 2
      ;;
    --help|-h)
      echo "Usage: install.sh [OPTIONS]"
      echo ""
      echo "Options:"
      echo "  --tool, -t     Tool to install: bramble or wt (default: both)"
      echo "  --version, -v  Version to install (default: latest)"
      echo "  --dir, -d      Installation directory (default: ~/.local/bin)"
      echo "  --help, -h     Show this help"
      exit 0
      ;;
    *)
      TOOL="$1"
      shift
      ;;
  esac
done

detect_os() {
  local os
  os="$(uname -s)"
  case "${os}" in
    Linux*)  echo "linux" ;;
    Darwin*) echo "darwin" ;;
    *)       echo "Unsupported OS: ${os}" >&2; exit 1 ;;
  esac
}

detect_arch() {
  local arch
  arch="$(uname -m)"
  case "${arch}" in
    x86_64|amd64)   echo "amd64" ;;
    arm64|aarch64)   echo "arm64" ;;
    *)               echo "Unsupported architecture: ${arch}" >&2; exit 1 ;;
  esac
}

get_latest_version() {
  curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name"' \
    | sed -E 's/.*"tag_name":\s*"([^"]+)".*/\1/'
}

verify_checksum() {
  local file="$1"
  local expected_sha="$2"
  local actual_sha

  if command -v sha256sum &>/dev/null; then
    actual_sha="$(sha256sum "${file}" | awk '{print $1}')"
  elif command -v shasum &>/dev/null; then
    actual_sha="$(shasum -a 256 "${file}" | awk '{print $1}')"
  else
    echo "  Warning: Cannot verify checksum (no sha256sum or shasum found)"
    return 2
  fi

  if [[ "${actual_sha}" != "${expected_sha}" ]]; then
    echo "Error: Checksum verification failed for ${file}"
    echo "  Expected: ${expected_sha}"
    echo "  Got:      ${actual_sha}"
    return 1
  fi
}

main() {
  local os arch
  os="$(detect_os)"
  arch="$(detect_arch)"

  if [[ -z "${VERSION}" ]]; then
    echo "Fetching latest version..."
    VERSION="$(get_latest_version)"
  fi

  echo "Platform: ${os}/${arch}"
  echo "Version:  ${VERSION}"

  # Determine which tools to install
  local tools=()
  if [[ -n "${TOOL}" ]]; then
    tools=("${TOOL}")
  else
    tools=("bramble" "wt")
  fi

  # Download checksums
  local checksums_url="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"
  local checksums_file
  checksums_file="$(mktemp)"
  trap "rm -f '${checksums_file}'" EXIT

  echo "Downloading checksums..."
  curl -fsSL "${checksums_url}" -o "${checksums_file}"

  mkdir -p "${INSTALL_DIR}"

  for tool in "${tools[@]}"; do
    local archive_name="${tool}-${VERSION}-${os}-${arch}.tar.gz"
    local download_url="https://github.com/${REPO}/releases/download/${VERSION}/${archive_name}"

    echo ""
    echo "Installing ${tool}..."
    echo "  Downloading ${archive_name}..."

    local tmp_dir
    tmp_dir="$(mktemp -d)"

    curl -fsSL "${download_url}" -o "${tmp_dir}/${archive_name}"

    # Verify checksum
    local expected_sha
    expected_sha="$(grep -F "${archive_name}" "${checksums_file}" | awk '{print $1}' || true)"
    if [[ -n "${expected_sha}" ]]; then
      local checksum_rc=0
      verify_checksum "${tmp_dir}/${archive_name}" "${expected_sha}" || checksum_rc=$?
      if [[ "${checksum_rc}" -eq 0 ]]; then
        echo "  Checksum verified."
      elif [[ "${checksum_rc}" -eq 2 ]]; then
        echo "  Skipping checksum verification."
      else
        exit 1
      fi
    fi

    tar -xzf "${tmp_dir}/${archive_name}" -C "${tmp_dir}"
    install -m 755 "${tmp_dir}/${tool}" "${INSTALL_DIR}/${tool}"
    rm -rf "${tmp_dir}"

    echo "  Installed ${tool} to ${INSTALL_DIR}/${tool}"
  done

  echo ""
  echo "Installation complete!"

  if [[ ":${PATH}:" != *":${INSTALL_DIR}:"* ]]; then
    echo ""
    echo "NOTE: ${INSTALL_DIR} is not in your PATH."
    echo "Add this to your shell profile:"
    echo "  export PATH=\"${INSTALL_DIR}:\${PATH}\""
  fi
}

main
