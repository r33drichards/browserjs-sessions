#!/usr/bin/env bash
# Generates one language's UniFFI bindings from the library built from this
# tree, and optionally checks and tests them.
#
#   scripts/bindings.sh <python|javascript|go> [--check] [--test]
#
#   (no flag)  write the generated sources into the tree
#   --check    do not write: fail if the tree's generated sources differ
#   --test     run the language's smoke test against the built library
#
# Run it inside the flake's `sdk` dev shell: `nix develop ..#sdk -c scripts/bindings.sh go --test`.
#
# The generators are pinned to the UniFFI release in Cargo.toml (0.31.0):
#   Python      uniffi-bindgen, the workspace's own binary (maturin runs it)
#   JavaScript  uniffi-bindgen-react-native ("ubrn"), its Node (N-API) target
#   Go          uniffi-bindgen-go (NordSecurity)
set -euo pipefail

GO_BINDGEN_TAG="v0.7.1+v0.31.0"
GO_BINDGEN_VERSION="uniffi-bindgen 0.7.1+v0.31.0"
UBRN_PACKAGE="uniffi-bindgen-react-native@0.31.0-6"

sdk="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
language="${1:-}"
shift || true
check=false
test=false
for flag in "$@"; do
  case "$flag" in
    --check) check=true ;;
    --test) test=true ;;
    *) echo "unknown flag: $flag" >&2; exit 2 ;;
  esac
done

target="${CARGO_TARGET_DIR:-$sdk/target}"
case "$(uname -s)" in
  Darwin) library="$target/debug/libcomputeruse.dylib"; library_path_var=DYLD_LIBRARY_PATH ;;
  Linux) library="$target/debug/libcomputeruse.so"; library_path_var=LD_LIBRARY_PATH ;;
  *) echo "unsupported host: $(uname -s)" >&2; exit 1 ;;
esac

work="$(mktemp -d "${TMPDIR:-/tmp}/computeruse-bindings.XXXXXX")"
trap 'rm -rf "$work"' EXIT

build_library() {
  (cd "$sdk" && cargo build --locked -p computeruse-sdk)
  [ -f "$library" ] || { echo "the library was not built: $library" >&2; exit 1; }
}

# install <generated dir> <tree dir> <file>...: with --check, compare the
# files; otherwise copy them into the tree. What else is in the tree's
# directory (hand-written files) is left alone. $GENERATED_OUT, when set,
# also receives the generated files: CI uploads it when a check fails.
install() {
  local generated="$1" tree="$2" stale=0 file
  shift 2
  if [ -n "${GENERATED_OUT:-}" ]; then
    mkdir -p "$GENERATED_OUT"
    cp -R "$generated/." "$GENERATED_OUT/"
  fi
  for file in "$@"; do
    [ -f "$generated/$file" ] || { echo "error: the generator did not write $file" >&2; ls -R "$generated" >&2; exit 1; }
    if $check; then
      diff -u "$tree/$file" "$generated/$file" > /dev/null 2>&1 || { echo "stale: $tree/$file" >&2; stale=1; }
    else
      mkdir -p "$tree"
      cp "$generated/$file" "$tree/$file"
      echo "wrote $tree/$file"
    fi
  done
  if [ "$stale" = 1 ]; then
    echo "error: generated sources in the tree are not what the generator produces; run scripts/bindings.sh $language" >&2
    exit 1
  fi
  if $check; then echo "ok: $tree is current"; fi
}

case "$language" in
  python)
    # Nothing generated is kept in the tree: maturin runs uniffi-bindgen and
    # puts the module and the library into the wheel.
    if $test; then
      (cd "$sdk/python" && maturin build --locked --out "$work/dist")
      python3 -m venv "$work/venv"
      "$work/venv/bin/pip" install --quiet --no-index --find-links "$work/dist" computeruse
      (cd "$sdk/python/tests" && "$work/venv/bin/python" -m unittest -v test_smoke)
    else
      build_library
      (cd "$sdk" && cargo run --quiet --locked -p uniffi-bindgen -- generate --library "$library" --language python --out-dir "$work/python")
      echo "generated (not kept; maturin does this when it builds the wheel):"
      ls -l "$work/python"
    fi
    ;;

  go)
    build_library
    bindgen="$(command -v uniffi-bindgen-go || true)"
    if [ -z "$bindgen" ] || [ "$("$bindgen" --version)" != "$GO_BINDGEN_VERSION" ]; then
      cargo install uniffi-bindgen-go --locked --debug \
        --git https://github.com/NordSecurity/uniffi-bindgen-go --tag "$GO_BINDGEN_TAG" \
        --root "$target/tools"
      bindgen="$target/tools/bin/uniffi-bindgen-go"
    fi
    "$bindgen" "$library" --library --out-dir "$work/go"
    gofmt -w "$work/go/computeruse"
    install "$work/go/computeruse" "$sdk/go/computeruse" computeruse.go computeruse.h
    if $test; then
      (cd "$sdk/go" \
        && test -z "$(gofmt -l . | tee /dev/stderr)" \
        && go vet ./... \
        && CGO_LDFLAGS="-L$target/debug" env "$library_path_var=$target/debug" go test -count=1 ./...)
    fi
    ;;

  javascript)
    build_library
    (cd "$sdk/js" && npm ci --no-audit --no-fund)
    # From the workspace: ubrn asks cargo about the crate behind the library.
    (cd "$sdk" && npx --yes --package "$UBRN_PACKAGE" ubrn \
      generate napi bindings "$library" --library --no-format \
      --ts-dir "$work/js" --lib-package-base computeruse/prebuilds/ --lib-node-triple)
    install "$work/js" "$sdk/js/src/generated" computeruse.ts computeruse-ffi.ts
    if $test; then
      # The package finds its library at prebuilds/<platform>-<arch>[-gnu]/.
      triple="$(node -p 'process.platform + "-" + process.arch + (process.platform === "linux" ? "-gnu" : "")')"
      mkdir -p "$sdk/js/prebuilds/$triple"
      cp "$library" "$sdk/js/prebuilds/$triple/"
      printf '{"name": "computeruse-%s", "version": "0.0.0", "private": true}\n' "$triple" > "$sdk/js/prebuilds/$triple/package.json"
      (cd "$sdk/js" && npm run typecheck && npm run build && node --test test/smoke.test.mjs)
    fi
    ;;

  *)
    echo "usage: scripts/bindings.sh <python|javascript|go> [--check] [--test]" >&2
    exit 2
    ;;
esac
