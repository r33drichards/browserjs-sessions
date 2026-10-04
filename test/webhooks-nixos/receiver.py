"""HTTPS receiver: verify signatures, retain raw deliveries, deduplicate effects."""
import asyncio
import hashlib
import hmac
import json
import sqlite3
import ssl
from aiohttp import web

SECRET = b'container-signing-secret'
db = sqlite3.connect('/var/lib/webhook-receiver/receipts.sqlite')
db.executescript('''CREATE TABLE IF NOT EXISTS attempts (body TEXT, batch_id TEXT);
CREATE TABLE IF NOT EXISTS effects (id TEXT PRIMARY KEY, event TEXT);''')
mode = 'accept'

async def receive(request):
    raw = await request.read()
    stamp = request.headers.get('X-Computer-Use-Timestamp', '')
    expected = 'sha256=' + hmac.new(SECRET, stamp.encode() + b'.' + raw, hashlib.sha256).hexdigest()
    if not hmac.compare_digest(request.headers.get('X-Computer-Use-Signature', ''), expected):
        return web.Response(status=401)
    doc = json.loads(raw)
    assert request.headers['X-Computer-Use-Batch-ID'] == doc['batch_id']
    with db:
        db.execute('INSERT INTO attempts VALUES (?,?)', (raw.decode(), doc['batch_id']))
        if mode != 'fail':
            for event in doc['events']:
                db.execute('INSERT OR IGNORE INTO effects VALUES (?,?)', (event['id'], json.dumps(event)))
    if mode == 'fail':
        return web.Response(status=503)
    if mode == 'lost_ack':
        await asyncio.sleep(600)  # receiver committed, sender never sees acknowledgement
    return web.Response(status=204)

async def control(request):
    global mode
    if request.method == 'PUT':
        mode = (await request.json())['mode']
    return web.json_response({'mode': mode,
        'attempts': [{'body': body, 'batch_id': bid} for body, bid in db.execute('SELECT * FROM attempts')],
        'effects': [json.loads(row[0]) for row in db.execute('SELECT event FROM effects')]})

async def main():
    app = web.Application()
    app.router.add_post('/events', receive)
    runner = web.AppRunner(app)
    await runner.setup()
    tls = ssl.create_default_context(ssl.Purpose.CLIENT_AUTH)
    tls.load_cert_chain('/var/lib/webhook-tls/cert.pem', '/var/lib/webhook-tls/key.pem')
    await web.TCPSite(runner, '0.0.0.0', 443, ssl_context=tls).start()
    admin = web.Application()
    admin.router.add_route('*', '/state', control)
    admin_runner = web.AppRunner(admin)
    await admin_runner.setup()
    await web.TCPSite(admin_runner, '127.0.0.1', 9000).start()
    await asyncio.Event().wait()

asyncio.run(main())
