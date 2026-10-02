#!/usr/bin/env bash
# Runs the SDK's live tests: the real API, a real session, in each language.
#
#   scripts/live.sh [rust] [python] [javascript] [go]      (default: all four)
#
# Run it inside the dev shell: nix develop ..#sdk -c scripts/live.sh
#
# The token is an API token with every scope (sessions:read, sessions:write,
# sessions:connect, policies:read, policies:write). It comes from
# COMPUTERUSE_API_TOKEN if that is set, and otherwise, on macOS, from the
# Keychain item with service `computeruse-api-token`:
#
#   security add-generic-password -s computeruse-api-token -a computeruse -w
#
# It is given to the test processes in their environment and nowhere else:
# never printed, never written to a file.
#
# COMPUTERUSE_BASE_URL names another deployment (default
# https://api.computeruse.site). Each language creates one session and
# deletes it; a run takes a few minutes per language. This is not part of
# CI: there is no token there.
set -euo pipefail

sdk="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ -z "${COMPUTERUSE_API_TOKEN:-}" ]; then
  if ! command -v security > /dev/null 2>&1; then
    echo "set COMPUTERUSE_API_TOKEN" >&2
    exit 2
  fi
  if ! COMPUTERUSE_API_TOKEN="$(security find-generic-password -s computeruse-api-token -w 2> /dev/null)"; then
    echo "no token: set COMPUTERUSE_API_TOKEN, or add the Keychain item computeruse-api-token" >&2
    exit 2
  fi
fi
export COMPUTERUSE_API_TOKEN COMPUTERUSE_LIVE=1

languages=("$@")
[ ${#languages[@]} -gt 0 ] || languages=(rust python javascript go)

target="${CARGO_TARGET_DIR:-$sdk/target}"
case "$(uname -s)" in
  Darwin) library="$target/debug/libcomputeruse.dylib"; library_path_var=DYLD_LIBRARY_PATH ;;
  *) library="$target/debug/libcomputeruse.so"; library_path_var=LD_LIBRARY_PATH ;;
esac

work="$(mktemp -d "${TMPDIR:-/tmp}/computeruse-live.XXXXXX")"
trap 'rm -rf "$work"' EXIT
failed=()

for language in "${languages[@]}"; do
  echo "=== $language"
  case "$language" in
    rust)
      (cd "$sdk" && cargo test --locked -p computeruse-sdk --test live -- --nocapture) || failed+=(rust)
      ;;
    python)
      (
        cd "$sdk" && cargo build --locked -p computeruse-sdk
        cargo run --quiet --locked -p uniffi-bindgen -- generate --library "$library" --language python --out-dir "$work/python/computeruse"
        cp "$library" python/python/computeruse/__init__.py python/python/computeruse/py.typed "$work/python/computeruse/"
        cd python/tests && PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$work/python" python3 -m unittest -v test_live
      ) || failed+=(python)
      ;;
    javascript)
      (
        cd "$sdk" && cargo build --locked -p computeruse-sdk
        triple="$(node -p 'process.platform + "-" + process.arch + (process.platform === "linux" ? "-gnu" : "")')"
        mkdir -p "js/prebuilds/$triple" && cp "$library" "js/prebuilds/$triple/"
        printf '{"name": "computeruse-%s", "version": "0.0.0", "private": true}\n' "$triple" > "js/prebuilds/$triple/package.json"
        cd js && npm ci --no-audit --no-fund > /dev/null && npm run build > /dev/null && node --test test/live.test.mjs
      ) || failed+=(javascript)
      ;;
    go)
      (
        cd "$sdk" && cargo build --locked -p computeruse-sdk
        cd go && CGO_LDFLAGS="-L$target/debug" env "$library_path_var=$target/debug" go test -count=1 -timeout 30m -run TestLive -v ./...
      ) || failed+=(go)
      ;;
    *)
      echo "unknown language: $language" >&2
      exit 2
      ;;
  esac
done

if [ ${#failed[@]} -gt 0 ]; then
  echo "failed: ${failed[*]}" >&2
  exit 1
fi
echo "live: all passed (${languages[*]})"
