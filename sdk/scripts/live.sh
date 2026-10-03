#!/usr/bin/env bash
# Runs the SDK's live tests: the real API, a real session, in each language.
#
#   scripts/live.sh [--build] [rust] [python] [javascript] [go]   (default: all four)
#
#   --build   build what each language's test needs, and run nothing: no
#             token is needed. A later run without it starts at once.
#
# Run it inside the dev shell: nix develop ..#sdk -c scripts/live.sh
# CI runs it from .github/workflows/sdk-live.yml with a token it makes for
# the run and deletes afterwards (hack/mint-token.sh).
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
# deletes it; a run takes a few minutes per language.
set -euo pipefail

sdk="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

build_only=false
languages=()
for argument in "$@"; do
  case "$argument" in
    --build) build_only=true ;;
    rust | python | javascript | go) languages+=("$argument") ;;
    *) echo "unknown argument: $argument" >&2; exit 2 ;;
  esac
done
[ ${#languages[@]} -gt 0 ] || languages=(rust python javascript go)

target="${CARGO_TARGET_DIR:-$sdk/target}"
case "$(uname -s)" in
  Darwin) library="$target/debug/libcomputeruse.dylib"; library_path_var=DYLD_LIBRARY_PATH ;;
  *) library="$target/debug/libcomputeruse.so"; library_path_var=LD_LIBRARY_PATH ;;
esac
# The Python module, generated from the library; kept between --build and
# the run.
python_dir="$target/live-python"

build() {
  case "$1" in
    rust)
      (cd "$sdk" && cargo test --locked -p computeruse-sdk --test live --no-run)
      ;;
    python)
      (cd "$sdk" && cargo build --locked -p computeruse-sdk)
      rm -rf "$python_dir" && mkdir -p "$python_dir/computeruse"
      (cd "$sdk" && cargo run --quiet --locked -p uniffi-bindgen -- generate --library "$library" --language python --out-dir "$python_dir/computeruse")
      cp "$library" "$sdk/python/python/computeruse/__init__.py" "$sdk/python/python/computeruse/py.typed" "$python_dir/computeruse/"
      ;;
    javascript)
      (cd "$sdk" && cargo build --locked -p computeruse-sdk)
      local triple
      triple="$(node -p 'process.platform + "-" + process.arch + (process.platform === "linux" ? "-gnu" : "")')"
      mkdir -p "$sdk/js/prebuilds/$triple" && cp "$library" "$sdk/js/prebuilds/$triple/"
      printf '{"name": "computeruse-%s", "version": "0.0.0", "private": true}\n' "$triple" > "$sdk/js/prebuilds/$triple/package.json"
      (cd "$sdk/js" && npm ci --no-audit --no-fund > /dev/null && npm run build > /dev/null)
      ;;
    go)
      (cd "$sdk" && cargo build --locked -p computeruse-sdk)
      (cd "$sdk/go" && CGO_LDFLAGS="-L$target/debug" go test -count=1 -run '^$' ./... > /dev/null)
      ;;
  esac
}

run() {
  case "$1" in
    rust)
      (cd "$sdk" && cargo test --locked -p computeruse-sdk --test live -- --nocapture)
      ;;
    python)
      (cd "$sdk/python/tests" && PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$python_dir" python3 -m unittest -v test_live)
      ;;
    javascript)
      (cd "$sdk/js" && node --test test/live.test.mjs)
      ;;
    go)
      (cd "$sdk/go" && CGO_LDFLAGS="-L$target/debug" env "$library_path_var=$target/debug" go test -count=1 -timeout 30m -run TestLive -v ./...)
      ;;
  esac
}

if $build_only; then
  for language in "${languages[@]}"; do
    echo "=== build $language"
    build "$language"
  done
  echo "live: built (${languages[*]})"
  exit 0
fi

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

failed=()
for language in "${languages[@]}"; do
  echo "=== $language"
  if ! { build "$language" && run "$language"; }; then
    failed+=("$language")
  fi
done

if [ ${#failed[@]} -gt 0 ]; then
  echo "failed: ${failed[*]}" >&2
  exit 1
fi
echo "live: all passed (${languages[*]})"
