#!/usr/bin/env bash
# Bring up the whole system on a local kind cluster. Safe to run again: it
# creates what is missing and updates the rest.
#
#   nix develop -c hack/local-up.sh
#
# Needs a running Docker (colima) and, for sign-in with Google and GitHub,
# the OAuth app credentials in the macOS Keychain (docs/local-development.md).
set -euo pipefail
. "$(dirname "$0")/lib.sh"

# Agent Sandbox release. v1.0.5 was published on 2026-10-01 without its
# controller image (registry.k8s.io/agent-sandbox/agent-sandbox-controller:v1.0.5
# did not exist yet), so the default is the release before it.
SANDBOX_VERSION="${SANDBOX_VERSION:-v1.0.4}"
mkdir -p "$LOCAL_DIR"

# --- cluster ---------------------------------------------------------------
if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  check_disk "creating the cluster"
  kind create cluster --name "$CLUSTER" --config hack/kind-config.yaml --kubeconfig "$KUBECONFIG"
else
  kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null
fi

kubectl apply --server-side -f \
  "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$SANDBOX_VERSION/sandbox-with-extensions.yaml"
kubectl wait --for=condition=Established crd/sandboxes.agents.x-k8s.io --timeout=120s

# --- images ----------------------------------------------------------------
image_id() { docker image inspect --format '{{.Id}}' "$1" 2>/dev/null || true; }

check_disk "building the backend image"
before=$(image_id browserjs/backend:dev)
# Without provenance the image's ID depends only on its contents, so an
# unchanged build is seen as unchanged below.
docker build --provenance=false -t browserjs/backend:dev .
backend_changed=""
[ "$before" = "$(image_id browserjs/backend:dev)" ] || backend_changed=1

# The session images are built once and reused: the browser image is a Nix
# build of several GB. Remove the image (or build it yourself) to refresh it.
if [ -z "$(image_id browserjs/mcp-js:dev)" ]; then
  docker build -t browserjs/mcp-js:dev images/mcp-js
fi
if [ -z "$(image_id browserjs/browser:dev)" ]; then
  MIN_FREE_GB=25 check_disk "building the browser image (about 14 GB)"
  docker build -t browserjs/browser:dev images/browser
fi

# The policy operator, once its source is in the tree. The context is the
# repository root: the image copies files of docs/contracts/policy.
have_operator=""
if [ -f images/policy-operator/Dockerfile ]; then
  have_operator=1
  docker build --provenance=false -t browserjs/policy-operator:dev -f images/policy-operator/Dockerfile .
fi

# kind does not recognise an image it already has when Docker uses the
# containerd image store, and would copy all of them (4 GB) every time. What
# was loaded is noted on the node itself, so the note goes with the cluster.
node="$CLUSTER-control-plane"
for image in backend mcp-js browser ${have_operator:+policy-operator}; do
  id=$(image_id "browserjs/$image:dev")
  if [ "$(docker exec "$node" cat "/kind/loaded-$image" 2>/dev/null)" = "$id" ]; then
    echo "browserjs/$image:dev is already on the node"
    continue
  fi
  check_disk "loading browserjs/$image:dev into the cluster"
  kind load docker-image "browserjs/$image:dev" --name "$CLUSTER"
  docker exec "$node" sh -c "echo '$id' > /kind/loaded-$image"
done

# --- certificates and secrets ------------------------------------------------
kubectl apply -f deploy/base/namespace.yaml

# A throwaway CA and one certificate for every local name. Never committed.
tls="$LOCAL_DIR/tls"
new_certificate=""
# A certificate from before the sessions had one host does not name it.
if [ -f "$tls/tls.crt" ] && ! openssl x509 -in "$tls/tls.crt" -noout -text | grep -q 'DNS:sessions\.localtest\.me'; then
  rm -f "$tls/tls.crt"
fi
if [ ! -f "$tls/tls.crt" ]; then
  new_certificate=1
  mkdir -p "$tls"
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 365 \
    -subj "/CN=browserjs sessions local CA" \
    -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" \
    -keyout "$tls/ca.key" -out "$tls/ca.crt" 2>/dev/null
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
    -subj "/CN=localtest.me" -keyout "$tls/tls.key" -out "$tls/tls.csr" 2>/dev/null
  printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\nkeyUsage=critical,digitalSignature\nbasicConstraints=critical,CA:FALSE\nauthorityKeyIdentifier=keyid\n' \
    "DNS:app.localtest.me,DNS:api.localtest.me,DNS:authenticate.localtest.me,DNS:sessions.localtest.me,DNS:*.sessions.localtest.me,DNS:pomerium.$NS.svc.cluster.local,DNS:pomerium.$NS.svc" >"$tls/ext.cnf"
  openssl x509 -req -in "$tls/tls.csr" -CA "$tls/ca.crt" -CAkey "$tls/ca.key" -CAcreateserial \
    -days 365 -extfile "$tls/ext.cnf" -out "$tls/tls.crt" 2>/dev/null
  chmod 600 "$tls"/*.key
fi
kubectl -n "$NS" create secret tls pomerium-tls --cert="$tls/tls.crt" --key="$tls/tls.key" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$NS" create configmap local-ca --from-file=ca.crt="$tls/ca.crt" \
  --dry-run=client -o yaml | kubectl apply -f -

# Secrets are passed on stdin, so they never appear in a command line, a
# file or this script's output.
secret_from_stdin() { # name; reads KEY=value lines
  kubectl -n "$NS" create secret generic "$1" --from-env-file=/dev/stdin --dry-run=client -o yaml |
    kubectl apply -f - >/dev/null
  echo "secret/$1 applied"
}
random_b64() { head -c 32 /dev/urandom | base64; }

# Pomerium's own secrets are made once: new ones would sign everyone out.
if ! kubectl -n "$NS" get secret pomerium >/dev/null 2>&1; then
  client_secret=$(random_b64 | tr -d '/+=')
  {
    echo "SHARED_SECRET=$(random_b64)"
    echo "COOKIE_SECRET=$(random_b64)"
    echo "SIGNING_KEY=$(openssl ecparam -genkey -name prime256v1 -noout | base64 | tr -d '\n')"
    echo "IDP_CLIENT_SECRET=$client_secret"
  } | secret_from_stdin pomerium
else
  client_secret=$(kubectl -n "$NS" get secret pomerium -o jsonpath='{.data.IDP_CLIENT_SECRET}' | base64 -d)
fi

# Session policies: the tokens OPA, the operator and the backend know each
# other by (docs/contracts/policy/deploy.md). Made once: all three read them
# at startup.
random_b64url() { head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n'; }
if ! kubectl -n "$NS" get secret policy-tokens >/dev/null 2>&1; then
  {
    echo "bundle-token=$(random_b64url)"
    echo "opa-token=$(random_b64url)"
    echo "operator-api-token=$(random_b64url)"
  } | secret_from_stdin policy-tokens
fi

# The Google and GitHub OAuth apps, from the Keychain. Without them Dex
# still starts, and the test users still work; those two buttons do not.
keychain() {
  security find-generic-password -s "browserjs-sessions-$1" -w 2>/dev/null || {
    echo "warning: no Keychain item browserjs-sessions-$1; that sign-in will not work" >&2
    echo "missing"
  }
}
{
  echo "GOOGLE_CLIENT_ID=$(keychain google-client-id)"
  echo "GOOGLE_CLIENT_SECRET=$(keychain google-client-secret)"
  echo "GITHUB_CLIENT_ID=$(keychain github-client-id)"
  echo "GITHUB_CLIENT_SECRET=$(keychain github-client-secret)"
  echo "POMERIUM_CLIENT_SECRET=$client_secret"
} | secret_from_stdin dex-oauth
unset client_secret

# --- the system ----------------------------------------------------------------
kubectl apply -k deploy/local
# The policy operator looks its resource up once, when it starts.
kubectl wait --for=condition=Established crd/sessionpolicies.browserjs.dev --timeout=60s
[ -z "$backend_changed" ] || kubectl -n "$NS" rollout restart deploy/backend
# A new CA: Pomerium must serve the new certificate and the backend trust it.
[ -z "$new_certificate" ] || kubectl -n "$NS" rollout restart statefulset/pomerium deploy/backend
# Dex and Pomerium read their Secrets at startup only.
kubectl -n "$NS" rollout status deploy/dex --timeout=300s
kubectl -n "$NS" rollout status statefulset/pomerium --timeout=300s
kubectl -n "$NS" rollout status deploy/backend --timeout=300s
kubectl -n agent-sandbox-system rollout status deploy/agent-sandbox-controller --timeout=300s
# Session policies (hack/policy-stage.sh). Off, both have no pods and this
# returns at once; OPA is ready only once it has the operator's bundle.
kubectl -n "$NS" rollout status deploy/policy-operator --timeout=300s
kubectl -n "$NS" rollout status deploy/opa --timeout=300s

cat <<EOF

Up.
  App:        https://app.localtest.me      (test users alice@example.com, bob@example.com,
                                             admin@example.com; password "test")
  Sessions:   https://sessions.localtest.me/<id>/mcp
  Dex:        http://localhost:5556/dex
  CA:         $tls/ca.crt   (the browser will warn unless you trust it;
                             MCP clients: NODE_EXTRA_CA_CERTS=$tls/ca.crt)
  kubectl:    export KUBECONFIG=$KUBECONFIG
  Tear down:  hack/local-down.sh
EOF
