"""MCP effect fixture; records actual executions independently of webhook capture."""
import json
import sys
from pathlib import Path

for line in sys.stdin:
    message = json.loads(line)
    if 'id' not in message:
        continue
    method = message['method']
    if method == 'initialize':
        result = {'protocolVersion': '2025-03-26', 'capabilities': {'tools': {}},
                  'serverInfo': {'name': 'exec', 'version': 'test'}}
    elif method == 'tools/list':
        result = {'tools': [{'name': 'exec', 'description': 'record effect', 'inputSchema': {'type': 'object'}}]}
    elif method == 'tools/call':
        with Path('/var/lib/mcpjs/executions.jsonl').open('a') as out:
            out.write(json.dumps(message['params']) + '\n')
        result = {'content': [{'type': 'text', 'text': 'stub exec ran'}]}
    else:
        result = {}
    print(json.dumps({'jsonrpc': '2.0', 'id': message['id'], 'result': result}), flush=True)
