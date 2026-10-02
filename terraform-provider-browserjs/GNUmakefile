# Run through the repository's SDK dev shell, which has Go, a C compiler and
# cargo: nix develop ..#sdk -c make <target>
# (docs, testacc and e2e also need tofu: nix shell nixpkgs#opentofu).
#
# The provider calls the API through the Computer Use SDK (../sdk/go), which
# is Go bindings to a Rust library. So every go command here needs cgo and
# the static library libcomputeruse.a, which `make lib` builds from ../sdk
# into .lib/. The targets below depend on it; a bare `go build` needs
# CGO_LDFLAGS="-L$PWD/.lib" set by hand.

TOFU ?= tofu
VERSION ?= 0.1.0
# The directory both tools search for providers without any configuration.
MIRROR ?= $(HOME)/.terraform.d/plugins
PLATFORM := $(shell go env GOOS)_$(shell go env GOARCH)
TFPLUGINDOCS_VERSION ?= v0.25.0

# Only the static library is put in .lib, so the linker cannot pick the
# shared one: a provider binary must run with nothing beside it.
LIB := $(CURDIR)/.lib/libcomputeruse.a
SDK := $(CURDIR)/../sdk
# release for a provider to install; debug builds in a third of the time.
SDK_PROFILE ?= release
SDK_SOURCES := $(shell find $(SDK)/crates -name '*.rs' -o -name Cargo.toml) $(SDK)/Cargo.toml $(SDK)/Cargo.lock

export CGO_ENABLED := 1
export CGO_LDFLAGS := -L$(CURDIR)/.lib $(CGO_LDFLAGS)

default: build

# The SDK's library, built from this repository's sdk/ with cargo.
lib: $(LIB)

$(LIB): $(SDK_SOURCES)
	cd $(SDK) && cargo build --locked -p computeruse-sdk $(if $(filter release,$(SDK_PROFILE)),--release,)
	mkdir -p $(dir $(LIB))
	cp $(SDK)/target/$(SDK_PROFILE)/libcomputeruse.a $(LIB)

build: lib
	go build -o terraform-provider-browserjs .

# Installs the provider as version $(VERSION) into a local mirror, under both
# registry hosts, so that `tofu init` and `terraform init` find it.
install: lib
	for host in registry.opentofu.org registry.terraform.io; do \
	  dir="$(MIRROR)/$$host/r33drichards/browserjs/$(VERSION)/$(PLATFORM)"; \
	  mkdir -p "$$dir" && \
	  go build -ldflags "-X main.version=$(VERSION)" -o "$$dir/terraform-provider-browserjs_v$(VERSION)" . || exit 1; \
	done

test: lib
	go vet ./...
	go test ./...

# Real plans and applies with OpenTofu, against the fake API unless
# BROWSERJS_ENDPOINT and BROWSERJS_TOKEN name a local deployment.
testacc: lib
	TF_ACC=1 TF_ACC_TERRAFORM_PATH="$$(command -v $(TOFU))" \
	TF_ACC_PROVIDER_NAMESPACE=hashicorp TF_ACC_PROVIDER_HOST=registry.opentofu.org \
	go test ./internal/provider -run TestAcc -count=1 -v

# plan, apply, edit, destroy of examples/session-policies with dev_overrides,
# against the fake API.
e2e: lib
	./hack/e2e.sh "$(TOFU)"

# The fake API, to try the provider by hand.
fakeapi: lib
	go run ./cmd/fakeapi -token bjs_fake_token

# docs/ is generated from the schema and examples/. The schema is read from
# the built provider through dev_overrides, so nothing is downloaded but
# tfplugindocs itself.
docs: build
	./hack/generate-docs.sh "$(TOFU)" "$(TFPLUGINDOCS_VERSION)"

fmt:
	gofmt -w .
	$(TOFU) fmt -recursive examples

.PHONY: default lib build install test testacc e2e fakeapi docs fmt
