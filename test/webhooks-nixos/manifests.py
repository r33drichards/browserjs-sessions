"""Render production manifests with offline images and test TLS/tool fixtures."""
import json
import hashlib
from datetime import datetime, timedelta, timezone
import os
from pathlib import Path
import yaml

root = Path(os.environ['SOURCE'])
docs = [{'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': 'browserjs-sessions'}}]
docs.extend(d for d in yaml.safe_load_all(Path(os.environ['SANDBOX_MANIFEST']).read_text()) if d)
for filename in ['crd-sessionpolicy.yaml', 'crd-apitoken.yaml', 'backend.yaml', 'networkpolicy.yaml', 'policy-operator.yaml', 'opa.yaml', 'webhook-redis.yaml']:
    docs.extend(d for d in yaml.safe_load_all((root / 'deploy/base' / filename).read_text()) if d)
for d in docs:
    if d['metadata'].get('namespace') == 'agent-sandbox-system' or d['kind'] == 'Namespace' and d['metadata']['name'] == 'agent-sandbox-system':
        if d['kind'] == 'Deployment':
            for c in d['spec']['template']['spec']['containers']:
                c['imagePullPolicy'] = 'Never'
        continue
    if d['kind'] not in ['Namespace', 'CustomResourceDefinition', 'ClusterRole', 'ClusterRoleBinding']:
        d['metadata']['namespace'] = 'browserjs-sessions'
    if d['kind'] in ['Deployment', 'StatefulSet']:
        for c in d['spec']['template']['spec']['containers']:
            c['image'] = 'webhooks/' + {'policy-operator': 'operator'}.get(c['name'], c['name']) + ':test'
            c['imagePullPolicy'] = 'Never'
            if c['name'] == 'backend':
                for e in c['env']:
                    if e['name'] == 'API_URL': e['value'] = 'https://api.example.test'
                    if e['name'] == 'ALLOWED_EMAILS': e['value'] = 'test@example.com'
            if c['name'] == 'policy-operator':
                c['env'].append({'name': 'SSL_CERT_FILE', 'value': '/tls/cert.pem'})
                c['volumeMounts'].append({'name': 'tls', 'mountPath': '/tls', 'readOnly': True})
                d['spec']['template']['spec']['volumes'].append({'name': 'tls', 'configMap': {'name': 'webhook-tls'}})


def obj(kind, name, **fields):
    return {'apiVersion': 'v1', 'kind': kind, 'metadata': {'name': name, 'namespace': 'browserjs-sessions'}, **fields}


docs += [obj('Secret', 'policy-tokens', stringData={
    'bundle-token': 'bundle-secret', 'opa-token': 'opa-secret', 'operator-api-token': 'redis-test-secret'}),
    obj('ConfigMap', 'webhook-tls', data={'cert.pem': Path('/var/lib/webhook-tls/cert.pem').read_text()}),
    obj('ConfigMap', 'opa-config', data={f: (root / 'docs/contracts/policy' / f).read_text()
                                       for f in ['opa-config.yaml', 'system-authz.rego']})]
# The cluster's real DNS answers the test-only public-looking destination.
# No application resolver or URL-validation override is installed.
docs.append({'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': 'coredns', 'namespace': 'kube-system'},
    'data': {'Corefile': '.:53 {\n errors\n health\n ready\n hosts {\n 93.184.216.34 webhook.example.test\n fallthrough\n }\n kubernetes cluster.local in-addr.arpa ip6.arpa {\n pods insecure\n fallthrough in-addr.arpa ip6.arpa\n }\n forward . /etc/resolv.conf\n cache 30\n loop\n reload\n loadbalance\n}\n'}})
policies = {'mcp_tools': {
    'pre': [{'url': 'http://policy-operator:8080', 'policy_path': 'browserjs/hooks/s-abcde/mcp_tools/pre'}],
    'policies': [{'url': 'http://opa:8181', 'policy_path': 'browserjs/decision/s-abcde/mcp_tools'}]}}
upstream = [{'name': 'exec', 'transport': 'stdio', 'command': os.environ['PYTHON'],
             'args': [str(root / 'test/webhooks-nixos/upstream.py')]}]
docs.append(obj('ConfigMap', 'mcpjs-config', data={'policies.json': json.dumps(policies), 'upstream.json': json.dumps(upstream)}))
# The backend renders the session ID into the same native hook/policy env
# used in production. Only the external browser/shell MCP implementation is a fixture.
policies['mcp_tools']['pre'][0]['policy_path'] = 'browserjs/hooks/{{ .ID }}/mcp_tools/pre'
policies['mcp_tools']['policies'][0]['policy_path'] = 'browserjs/decision/{{ .ID }}/mcp_tools'
blueprint = {'podTemplate': {
    'metadata': {'labels': {'app': 'browserjs-session'}},
    'spec': {'automountServiceAccountToken': False, 'enableServiceLinks': False,
        'containers': [{'name': 'mcp-js', 'image': 'webhooks/mcpjs:test', 'imagePullPolicy': 'Never',
            'env': [{'name': 'MCP_V8_POLICIES_JSON', 'value': json.dumps(policies)}],
            'args': ['--http-port', '8080', '--session-db-path', '/var/lib/mcpjs/sessions', '--mcp-config', '/config/upstream.json'],
            'ports': [{'name': 'mcp', 'containerPort': 8080}],
            'readinessProbe': {'tcpSocket': {'port': 8080}, 'periodSeconds': 1},
            'volumeMounts': [{'name': 'config', 'mountPath': '/config'}, {'name': 'state', 'mountPath': '/var/lib/mcpjs'}]}],
        'volumes': [{'name': 'config', 'configMap': {'name': 'mcpjs-config'}}, {'name': 'state', 'emptyDir': {}}]}}}
docs.append(obj('ConfigMap', 'session-blueprint', data={'blueprint.yaml': yaml.safe_dump(blueprint)}))
docs.append(obj('ConfigMap', 'billing-catalogue', data={'catalogue.yaml': (root / 'deploy/base/catalogue.yaml').read_text()}))
token = 'bjs_abcdefghijkl_' + 'a' * 43
docs.append({'apiVersion': 'browserjs.dev/v1alpha1', 'kind': 'APIToken',
    'metadata': {'name': 'tok-abcdefghijkl', 'namespace': 'browserjs-sessions'},
    'spec': {'id': 'abcdefghijkl', 'owner': 'test@example.com', 'name': 'integration',
        'scopes': ['sessions:read', 'sessions:write', 'sessions:connect', 'policies:read', 'policies:write'],
        'expiresAt': (datetime.now(timezone.utc) + timedelta(days=1)).strftime('%Y-%m-%dT%H:%M:%SZ'),
        'sha256': hashlib.sha256(token.encode()).hexdigest()}})
print(yaml.safe_dump_all(docs))
