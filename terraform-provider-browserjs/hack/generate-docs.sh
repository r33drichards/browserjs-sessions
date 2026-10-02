#!/usr/bin/env bash
# Regenerates docs/ from the provider's schema and examples/.
#   hack/generate-docs.sh [tofu|terraform] [tfplugindocs version]
set -euo pipefail

tf="${1:-tofu}"
version="${2:-v0.25.0}"
here="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

[ -x "$here/terraform-provider-browserjs" ] || (cd "$here" && go build -o terraform-provider-browserjs .)

cat > "$work/rc" <<RC
provider_installation {
  dev_overrides {
    "r33drichards/browserjs" = "$here"
  }
  direct {}
}
RC
cat > "$work/main.tf" <<'TF'
terraform {
  required_providers {
    browserjs = { source = "r33drichards/browserjs" }
  }
}
TF

# tfplugindocs looks the provider up as registry.terraform.io/hashicorp/<name>;
# the schema is keyed by the address in main.tf on whichever registry host the
# binary assumes. There is one provider, so rename its key.
(cd "$work" && TF_CLI_CONFIG_FILE="$work/rc" "$tf" providers schema -json |
  jq '.provider_schemas |= with_entries(.key = "registry.terraform.io/hashicorp/browserjs")' > schema.json)

cd "$here"
go run "github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs@$version" generate \
  --provider-name browserjs \
  --rendered-provider-name browserjs \
  --providers-schema "$work/schema.json"
