import json
import shlex

start_all()
for unit in ['redis-test', 'webhook-tls', 'webhook-receiver', 'webhook-collector', 'opa-test', 'mcpjs']:
    stack.wait_for_unit(unit + '.service')
for port in [6379, 443, 8080, 8181, 8088, 9000]:
    stack.wait_for_open_port(port)
stack.wait_until_succeeds('curl -fsS http://localhost:8080/readyz')

resource_path = '/var/lib/webhook-collector/resource.json'
resource = json.loads(stack.succeed('cat ' + resource_path))
original_webhook = dict(resource['spec']['webhook'])


def state():
    return json.loads(stack.succeed('curl -fsS http://localhost:9000/state'))


def mode(value):
    stack.succeed('curl -fsS -X PUT -H "Content-Type: application/json" -d '
                  + shlex.quote(json.dumps({'mode': value})) + ' http://localhost:9000/state')


def update():
    content = json.dumps(resource)
    # Atomic replacement: the collector never reads a partial resource.
    stack.succeed('printf %s ' + shlex.quote(content) + ' > ' + resource_path + '.tmp; mv '
                  + resource_path + '.tmp ' + resource_path)
    # Poll the reconciled webhook settings using their effect (below), rather
    # than assuming a sleep or systemd activation means the watcher applied it.


def verdict(allowed):
    doc = {'input': {'operation': 'mcp_call_tool', 'server': 'exec', 'tool': 'exec',
                     'arguments': {'bin': 'test', 'args': []}}}
    stack.wait_until_succeeds('curl -fsS -H "Content-Type: application/json" -d '
        + shlex.quote(json.dumps(doc)) + ' http://localhost:8181/v1/data/browserjs/decision/s-abcde/mcp_tools'
        + ' | jq -e ' + shlex.quote('.result.allow == ' + str(allowed).lower()))


def call(bin_name, expected):
    result = json.loads(stack.succeed('SERVER=exec python /etc/webhook-call.py http://localhost:8088 '
                                     + shlex.quote(bin_name)))
    assert result['outcome'] == expected, result


def effects(count):
    stack.wait_until_succeeds('curl -fsS http://localhost:9000/state | jq -e '
                             + shlex.quote('.effects | length == ' + str(count)))


def drained():
    stack.wait_until_succeeds('test "$(REDISCLI_AUTH=redis-test-secret redis-cli SCARD '
                             + shlex.quote('browserjs:{webhooks}:groups') + ')" = 0')


def executions():
    return int(stack.succeed("if [ -f /var/lib/mcpjs/executions.jsonl ]; then wc -l < /var/lib/mcpjs/executions.jsonl; else echo 0; fi"))


with subtest('native hook, full batch, HTTPS, HMAC, and actual execution'):
    verdict(True)
    call('first', 'ran')
    call('second', 'ran')
    effects(2)
    drained()
    batch = json.loads(state()['attempts'][0]['body'])
    assert len(batch['events']) == 2, batch
    assert all(e['stage'] == 'attempt' and 'allowed' not in e for e in batch['events'])
    assert executions() == 2

with subtest('denied attempts are exported, without executing the tool'):
    resource['spec']['source'] = 'package browserjs.policy\nimport rego.v1\nallow_tool_call := false\n'
    update()
    verdict(False)
    call('denied', 'denied')
    effects(3)  # partial batch flushes after its interval
    drained()
    assert executions() == 2
    assert state()['effects'][-1]['arguments']['bin'] == 'denied'

with subtest('Rego filter selects exports without denying execution'):
    resource['spec']['source'] = 'package browserjs.policy\nimport rego.v1\nallow_tool_call := true\n'
    resource['spec']['webhook']['filter'] = 'package browserjs.policy\nimport rego.v1\nallow_tool_call if input.arguments.bin == "keep"\n'
    update()
    verdict(True)
    call('exclude', 'ran')
    call('keep', 'ran')
    effects(4)
    drained()
    assert state()['effects'][-1]['arguments']['bin'] == 'keep'
    assert not any(e['arguments']['bin'] == 'exclude' for e in state()['effects'])
    assert executions() == 4

with subtest('lost acknowledgement replays identical batch; receiver effects deduplicate'):
    resource['spec']['webhook'] = dict(original_webhook)
    update()
    # Start a new collector after configuring; bootstrap validates the exact
    # persisted resource before becoming ready.
    stack.succeed('systemctl restart webhook-collector')
    stack.wait_until_succeeds('curl -fsS http://localhost:8080/readyz')
    mode('lost_ack')
    call('lost-ack', 'ran')
    effects(5)
    recorded = state()['attempts'][-1]
    stack.succeed('systemctl kill --signal=SIGKILL webhook-collector; systemctl stop webhook-collector')
    mode('accept')
    stack.succeed('systemctl start webhook-collector')
    drained()
    replayed = [a for a in state()['attempts'] if a['batch_id'] == recorded['batch_id']]
    assert len(replayed) >= 2 and all(a['body'] == recorded['body'] for a in replayed)
    assert len(state()['effects']) == 5

with subtest('Redis SIGKILL retains pending batch; Redis outage blocks execution'):
    mode('fail')
    call('redis-recovery', 'ran')
    stack.wait_until_succeeds('curl -fsS http://localhost:9000/state | jq -e '
                             + shlex.quote('.attempts[-1].body | contains("redis-recovery")'))
    recorded = state()['attempts'][-1]
    before = executions()
    stack.succeed('systemctl kill --signal=SIGKILL redis-test; systemctl stop redis-test')
    call('must-not-execute', 'denied')
    assert executions() == before
    stack.succeed('systemctl stop webhook-collector; systemctl start redis-test')
    stack.wait_for_open_port(6379)
    mode('accept')
    stack.succeed('systemctl start webhook-collector')
    effects(6)
    drained()
    replayed = [a for a in state()['attempts'] if a['batch_id'] == recorded['batch_id']]
    assert len(replayed) >= 2 and all(a['body'] == recorded['body'] for a in replayed)
    assert not any(e['arguments']['bin'] == 'must-not-execute' for e in state()['effects'])

with subtest('disabling exports preserves accepted backlog'):
    mode('fail')
    call('before-disable', 'ran')
    stack.wait_until_succeeds('curl -fsS http://localhost:9000/state | jq -e '
                             + shlex.quote('.attempts[-1].body | contains("before-disable")'))
    del resource['spec']['webhook']
    update()
    stack.succeed('systemctl restart webhook-collector')
    stack.wait_until_succeeds('curl -fsS http://localhost:8080/readyz')
    call('after-disable', 'ran')
    mode('accept')
    effects(7)
    drained()
    assert not any(e['arguments']['bin'] == 'after-disable' for e in state()['effects'])

stack.succeed('journalctl -u webhook-collector -u redis-test -u mcpjs --no-pager > /tmp/services.log')
stack.copy_from_machine('/tmp/services.log')
stack.succeed('curl -fsS http://localhost:9000/state > /tmp/receiver-state.json')
stack.copy_from_machine('/tmp/receiver-state.json')
