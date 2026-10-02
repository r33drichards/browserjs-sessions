# Phase 0 spike scripts

What was run for section 11 of the design, kept so it can be run again
(after an OPA upgrade, say). None of this is product code.

| File | What it does |
|---|---|
| `run-cases.py <opa> <contracts dir>` | every example's cases, through a tenant package and the decision module, under the capabilities file |
| `bundle-stub.py <port>` | a bundle server: serves `./bundle.tar.gz` with an ETag, long-polls on `Prefer: wait=N`, answers 503 while the file does not exist |
| `tenant-guard.py <opa>` | a sketch of tenant checks 1 to 3 with a corpus of modules they must refuse |

Two OPA servers against the stub, as in spikes 2 to 4 (run in an empty
directory; `opa-config.yaml` with the service URL changed to
`http://127.0.0.1:9080` and the `credentials` and `persist` lines removed):

```
python3 bundle-stub.py 9080 &
for p in 8181 8182; do
  OPERATOR_TOKEN=optoken opa run --server --addr 127.0.0.1:$p \
    --authentication=token --authorization=basic \
    -c opa-config.yaml ../system-authz.rego &
done
curl -s -o /dev/null -w '%{http_code}\n' '127.0.0.1:8181/health?bundles'     # 500: no bundle yet
# build a bundle directory as rego-contract.md lays it out, then:
opa build -b bundle-src --capabilities ../capabilities.json -r 1 -o bundle.tar.gz
curl -s -X POST 127.0.0.1:8181/v1/data/browserjs/decision/s-ab2cd/mcp_tools \
  -d "{\"input\": $(cat ../input-sample.json)}"
curl -s -H 'Authorization: Bearer optoken' 127.0.0.1:8181/v1/data/browserjs/loaded
```
