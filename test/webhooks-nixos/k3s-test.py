import json
import shlex

start_all()
cluster.wait_for_unit('k3s.service', timeout=180)
cluster.wait_for_unit('webhook-receiver.service')
cluster.wait_until_succeeds('kubectl get --raw=/readyz', timeout=180)
cluster.wait_until_succeeds("kubectl get nodes -o jsonpath='{.items[0].status.conditions[?(@.type==\"Ready\")].status}' | grep True", timeout=180)
cluster.succeed('/etc/render-webhooks > /tmp/webhooks.yaml; kubectl apply -f /tmp/webhooks.yaml')
k = 'kubectl -n browserjs-sessions '
cluster.wait_until_succeeds(k + 'rollout status statefulset/webhook-redis --timeout=10s', timeout=180)
cluster.wait_until_succeeds(k + 'rollout status deployment/policy-operator --timeout=10s', timeout=180)

cluster.wait_until_succeeds('kubectl -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=10s', timeout=180)
ready_backend = k + 'rollout status deployment/backend --timeout=10s'
cluster.wait_until_succeeds(ready_backend, timeout=180)
backend = cluster.succeed(k + 'get service backend -o jsonpath={.spec.clusterIP}').strip()
token = 'bjs_abcdefghijkl_' + 'a' * 43


def api(method, path, body=None):
    command = 'curl -fsS -X ' + method + ' -H "Host: api.example.test" -H '
    command += shlex.quote('Authorization: Bearer ' + token) + ' -H "Content-Type: application/json" '
    if body is not None:
        command += '-d ' + shlex.quote(json.dumps(body)) + ' '
    return cluster.succeed(command + shlex.quote('http://' + backend + path))


created = json.loads(api('POST', '/v1/sessions', {'name': 'webhook-integration', 'policy': {
    'kind': 'rego', 'source': 'package browserjs.policy\nimport rego.v1\nallow_tool_call := true\n'}}))
sid = created['id']
cluster.wait_until_succeeds(k + 'get sandbox ' + sid + ' -o json | jq -e '
    + shlex.quote('.status.conditions[] | select(.type == "Ready") | .status == "True"'), timeout=180)
resource = json.loads(cluster.succeed(k + 'get sessionpolicy ' + sid + ' -o json'))
resource['spec']['webhook'] = {'url': 'https://webhook.example.test/events', 'batch_size': 2,
    'flush_interval_seconds': 5, 'signing_secret': 'container-signing-secret', 'filter': ''}
original_webhook = dict(resource['spec']['webhook'])


def update():
    patch = {'spec': {'source': resource['spec']['source']}}
    cluster.succeed(k + 'patch sessionpolicy ' + sid + ' --type=merge -p ' + shlex.quote(json.dumps(patch)))
    path = '/v1/sessions/' + sid + '/webhook'
    if 'webhook' in resource['spec']:
        api('PUT', path, resource['spec']['webhook'])
        read = json.loads(api('GET', path))
        assert read['has_signing_secret'] and 'signing_secret' not in read, read
        stored = json.loads(cluster.succeed(k + 'get sessionpolicy ' + sid + ' -o json'))
        assert stored['spec']['webhook'] == resource['spec']['webhook']
    else:
        api('DELETE', path)


def state():
    return json.loads(cluster.succeed('curl -fsS http://localhost:9000/state'))


def mode(value):
    cluster.succeed('curl -fsS -X PUT -H "Content-Type: application/json" -d '
        + shlex.quote(json.dumps({'mode': value})) + ' http://localhost:9000/state')


def effects(count):
    cluster.wait_until_succeeds('curl -fsS http://localhost:9000/state | jq -e '
        + shlex.quote('.effects | length == ' + str(count)))


def drained():
    cluster.wait_until_succeeds(k + 'exec webhook-redis-0 -- sh -ec '
        + shlex.quote('test "$(redis-cli SCARD \'browserjs:{webhooks}:groups\')" = 0'))


def ready(name):
    cluster.wait_until_succeeds(k + 'rollout status deployment/' + name + ' --timeout=10s', timeout=180)


def restart(name):
    cluster.succeed(k + 'rollout restart deployment/' + name)
    ready(name)


def verdict(allowed):
    # Wait for the real CRD watcher and OPA bundle rollout, not a fixed delay.
    cluster.wait_until_succeeds(k + 'get sessionpolicy ' + sid + ' -o json | jq -e '
        + shlex.quote('.status.conditions[] | select(.type == "Ready") | .status == "True"'))
    address = cluster.succeed(k + 'get service opa -o jsonpath={.spec.clusterIP}').strip()
    doc = {'input': {'operation': 'mcp_call_tool', 'server': 'exec', 'tool': 'exec', 'arguments': {'bin': 'test', 'args': []}}}
    cluster.wait_until_succeeds('curl -fsS -H "Content-Type: application/json" -d '
        + shlex.quote(json.dumps(doc)) + ' http://' + address + ':8181/v1/data/browserjs/decision/' + sid + '/mcp_tools'
        + ' | jq -e ' + shlex.quote('.result.allow == ' + str(allowed).lower()))


def call(bin_name, expected):
    address = cluster.succeed(k + 'get pods -l app=browserjs-session -o jsonpath={.items[0].status.podIP}').strip()
    result = json.loads(cluster.succeed('SERVER=exec python /etc/webhook-call.py http://' + address + ':8080 ' + shlex.quote(bin_name)))
    assert result['outcome'] == expected, result


def pod():
    return cluster.succeed(k + 'get pods -l app=browserjs-session -o jsonpath={.items[0].metadata.name}').strip()


def executions():
    return int(cluster.succeed(k + 'exec ' + pod() + ' -- python -c '
        + shlex.quote('from pathlib import Path; p=Path("/var/lib/mcpjs/executions.jsonl"); print(len(p.read_text().splitlines()) if p.exists() else 0)')))


try:
    update()
    ready('opa')
    with subtest('real CRD watcher, Services, native hooks, signed HTTPS full batch'):
        verdict(True)
        call('first', 'ran')
        call('second', 'ran')
        effects(2)
        drained()
        batch = json.loads(state()['attempts'][0]['body'])
        assert len(batch['events']) == 2
        assert all(e['stage'] == 'attempt' and 'allowed' not in e for e in batch['events'])
        assert executions() == 2

    with subtest('policy updates deny execution while exporting denied attempts'):
        resource['spec']['source'] = 'package browserjs.policy\nimport rego.v1\nallow_tool_call := false\n'
        update()
        verdict(False)
        call('denied', 'denied')
        effects(3)
        drained()
        assert executions() == 2

    with subtest('Rego export filter is independent of execution authorization'):
        resource['spec']['source'] = 'package browserjs.policy\nimport rego.v1\nallow_tool_call := true\n'
        resource['spec']['webhook']['filter'] = 'package browserjs.policy\nimport rego.v1\nallow_tool_call if input.arguments.bin == "keep"\n'
        update()
        restart('policy-operator')
        verdict(True)
        call('exclude', 'ran')
        call('keep', 'ran')
        effects(4)
        drained()
        assert state()['effects'][-1]['arguments']['bin'] == 'keep'
        assert executions() == 4

    with subtest('lost acknowledgement survives operator pod deletion with identical replay'):
        resource['spec']['webhook'] = dict(original_webhook)
        update()
        restart('policy-operator')
        mode('lost_ack')
        call('lost-ack', 'ran')
        effects(5)
        recorded = state()['attempts'][-1]
        cluster.succeed(k + 'delete pod -l app=policy-operator --grace-period=0 --force')
        mode('accept')
        ready('policy-operator')
        drained()
        replayed = [a for a in state()['attempts'] if a['batch_id'] == recorded['batch_id']]
        assert len(replayed) >= 2 and all(a['body'] == recorded['body'] for a in replayed)
        assert len(state()['effects']) == 5

    with subtest('Redis PVC recovers pending batch after pod deletion; outage blocks execution'):
        mode('fail')
        call('redis-recovery', 'ran')
        cluster.wait_until_succeeds('curl -fsS http://localhost:9000/state | jq -e '
            + shlex.quote('.attempts[-1].body | contains("redis-recovery")'))
        recorded = state()['attempts'][-1]
        before = executions()
        cluster.succeed(k + 'scale statefulset/webhook-redis --replicas=0')
        cluster.wait_until_succeeds(k + 'get pods -l app=webhook-redis -o json | jq -e ".items | length == 0"')
        call('must-not-execute', 'denied')
        assert executions() == before
        mode('accept')
        cluster.succeed(k + 'scale statefulset/webhook-redis --replicas=1')
        cluster.wait_until_succeeds(k + 'rollout status statefulset/webhook-redis --timeout=10s', timeout=180)
        effects(6)
        drained()
        replayed = [a for a in state()['attempts'] if a['batch_id'] == recorded['batch_id']]
        assert len(replayed) >= 2 and all(a['body'] == recorded['body'] for a in replayed)
        assert not any(e['arguments']['bin'] == 'must-not-execute' for e in state()['effects'])

    with subtest('removing subscription preserves accepted Redis backlog'):
        mode('fail')
        call('before-disable', 'ran')
        cluster.wait_until_succeeds('curl -fsS http://localhost:9000/state | jq -e '
            + shlex.quote('.attempts[-1].body | contains("before-disable")'))
        del resource['spec']['webhook']
        update()
        restart('policy-operator')
        call('after-disable', 'ran')
        mode('accept')
        effects(7)
        drained()
        assert not any(e['arguments']['bin'] == 'after-disable' for e in state()['effects'])
finally:
    cluster.succeed('journalctl -u k3s --no-pager > /tmp/k3s.log')
    cluster.copy_from_machine('/tmp/k3s.log')
    cluster.succeed(k + 'get pods,pvc,sessionpolicies -o yaml > /tmp/resources.yaml')
    cluster.copy_from_machine('/tmp/resources.yaml')
    cluster.succeed(k + 'logs deployment/policy-operator --all-containers > /tmp/operator.log 2>&1 || true')
    cluster.copy_from_machine('/tmp/operator.log')
    cluster.succeed('curl -fsS http://localhost:9000/state > /tmp/receiver-state.json')
    cluster.copy_from_machine('/tmp/receiver-state.json')
