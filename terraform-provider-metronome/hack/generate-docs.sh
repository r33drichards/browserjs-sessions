#!/usr/bin/env bash
# Regenerates docs/ from the provider's schema and examples/.
#   hack/generate-docs.sh [tofu|terraform] [tfplugindocs version]
set -euo pipefail

tf="${1:-tofu}"
version="${2:-v0.25.0}"
here="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

[ -x "$here/terraform-provider-metronome" ] || (cd "$here" && go build -o terraform-provider-metronome .)

cat > "$work/rc" <<RC
provider_installation {
  dev_overrides {
    "r33drichards/metronome" = "$here"
  }
  direct {}
}
RC
cat > "$work/main.tf" <<'TF'
terraform {
  required_providers {
    metronome = { source = "r33drichards/metronome" }
  }
}
TF

# tfplugindocs looks the provider up as registry.terraform.io/hashicorp/<name>;
# the schema is keyed by the address in main.tf on whichever registry host the
# binary assumes. There is one provider, so rename its key.
(cd "$work" && TF_CLI_CONFIG_FILE="$work/rc" "$tf" providers schema -json |
  jq '.provider_schemas |= with_entries(.key = "registry.terraform.io/hashicorp/metronome")' > schema.json)

cd "$here"
go run "github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs@$version" generate \
  --provider-name metronome \
  --rendered-provider-name metronome \
  --providers-schema "$work/schema.json"
