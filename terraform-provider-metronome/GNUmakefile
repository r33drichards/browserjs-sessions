# Run through the repository's Nix dev shell: nix develop -c make <target>
# (docs, testacc and e2e also need tofu: nix shell nixpkgs#opentofu).

TOFU ?= tofu
VERSION ?= 0.1.0
# The directory both tools search for providers without any configuration.
MIRROR ?= $(HOME)/.terraform.d/plugins
PLATFORM := $(shell go env GOOS)_$(shell go env GOARCH)
TFPLUGINDOCS_VERSION ?= v0.25.0

default: build

build:
	go build -o terraform-provider-metronome .

# Installs the provider as version $(VERSION) into a local mirror, under both
# registry hosts, so that `tofu init` and `terraform init` find it.
install:
	for host in registry.opentofu.org registry.terraform.io; do \
	  dir="$(MIRROR)/$$host/r33drichards/metronome/$(VERSION)/$(PLATFORM)"; \
	  mkdir -p "$$dir" && \
	  go build -ldflags "-X main.version=$(VERSION)" -o "$$dir/terraform-provider-metronome_v$(VERSION)" . || exit 1; \
	done

test:
	go vet ./...
	go test ./...

# Real plans, applies and imports with OpenTofu, against the fake API. The
# same journey runs against Metronome itself only when
# METRONOME_ACC_BEARER_TOKEN holds a sandbox token.
testacc:
	TF_ACC=1 TF_ACC_TERRAFORM_PATH="$$(command -v $(TOFU))" \
	TF_ACC_PROVIDER_NAMESPACE=hashicorp TF_ACC_PROVIDER_HOST=registry.opentofu.org \
	go test ./internal/provider -run TestAcc -count=1 -v

# plan, apply, edit, destroy of examples/computer-use with dev_overrides,
# against the fake API.
e2e:
	./hack/e2e.sh "$(TOFU)"

# The fake API, to try the provider by hand.
fakeapi:
	go run ./cmd/fakeapi -token fake-token

# docs/ is generated from the schema and examples/. The schema is read from
# the built provider through dev_overrides, so nothing is downloaded but
# tfplugindocs itself.
docs: build
	./hack/generate-docs.sh "$(TOFU)" "$(TFPLUGINDOCS_VERSION)"

fmt:
	gofmt -w .
	$(TOFU) fmt -recursive examples

.PHONY: default build install test testacc e2e fakeapi docs fmt
