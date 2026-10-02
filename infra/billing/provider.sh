#!/usr/bin/env bash
# Builds terraform-provider-metronome from this repository into a filesystem
# mirror and writes an OpenTofu CLI configuration that takes that one
# provider from the mirror and every other from the registry. Prints the
# configuration's path:
#
#   export TF_CLI_CONFIG_FILE="$(infra/billing/provider.sh)"
#   tofu -chdir=infra/billing init -backend=false
#
# Needs go (nix develop). The mirror is $BILLING_PROVIDER_DIR, by default
# .provider/ beside this script (ignored by git).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
dir="${BILLING_PROVIDER_DIR:-$here/.provider}"
mkdir -p "$dir"
dir="$(cd "$dir" && pwd)"

make -C "$here/../../terraform-provider-metronome" install MIRROR="$dir/mirror" VERSION=0.1.0 >&2

cat > "$dir/tofurc" <<RC
provider_installation {
  filesystem_mirror {
    path    = "$dir/mirror"
    include = ["registry.opentofu.org/r33drichards/metronome"]
  }
  direct {
    exclude = ["registry.opentofu.org/r33drichards/metronome"]
  }
}
RC
echo "$dir/tofurc"
