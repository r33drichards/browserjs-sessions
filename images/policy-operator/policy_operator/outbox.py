"""Redis Streams outbox: atomic queue mutations, acknowledged only after AOF fsync."""
from __future__ import annotations
import hashlib
import json
import uuid
import redis

MAX_PENDING_BYTES = 256 * 1024 * 1024

class Outbox:
    def __init__(self, url: str, prefix: str = 'browserjs:{webhooks}:', password: str = ''):
        self.prefix = prefix
        self.db = redis.Redis.from_url(url, password=password or None, decode_responses=True,
                                       socket_timeout=5, socket_connect_timeout=2)
        # Never silently downgrade the acceptance guarantee on an external Redis.
        settings = self.db.config_get('append*')
        if settings.get('appendonly') != 'yes' or settings.get('appendfsync') != 'always':
            raise RuntimeError('webhook Redis requires appendonly yes and appendfsync always')
        if self.db.config_get('maxmemory-policy').get('maxmemory-policy') != 'noeviction':
            raise RuntimeError('webhook Redis requires maxmemory-policy noeviction')
        self.db.execute_command('WAITAOF', 1, 0, 2000)

    def key(self, name):
        return self.prefix + name

    def write(self, script, *args):
        # A non-transactional pipeline keeps EVAL and WAITAOF on the same connection.
        with self.db.pipeline(transaction=False) as pipe:
            pipe.eval(script, 1, self.prefix, *args)
            pipe.execute_command('WAITAOF', 1, 0, 2000)
            result, synced = pipe.execute()
        if synced[0] != 1:
            raise redis.RedisError('webhook Redis AOF acknowledgement timed out')
        return result

    def ingest(self, events):
        rows = []
        for event, cfg in events:
            sid = event['session_id']
            config = json.dumps(cfg, sort_keys=True, separators=(',', ':'))
            group = hashlib.sha256((sid + '\0' + config).encode()).hexdigest()
            payload = json.dumps(event, separators=(',', ':'))
            rows.append([sid + '/' + event['id'], group, json.dumps([sid, cfg]), payload, len(payload.encode())])
        return bool(self.write('''
local p=KEYS[1]; local rows=cjson.decode(ARGV[1]); local fresh={}; local seen={}
local size=tonumber(redis.call('GET',p..'bytes') or '0')
for _,r in ipairs(rows) do
 if not seen[r[1]] and redis.call('HEXISTS',p..'receipts',r[1])==0 then
  seen[r[1]]=true; size=size+r[5]; table.insert(fresh,r)
 end
end
if size>tonumber(ARGV[2]) then return 0 end
for _,r in ipairs(fresh) do
 redis.call('HSET',p..'destinations',r[2],r[3])
 redis.call('XADD',p..'queue:'..r[2],'*','payload',r[4],'size',r[5])
 redis.call('HSET',p..'receipts',r[1],1)
 redis.call('SADD',p..'groups',r[2])
end
redis.call('SET',p..'bytes',size); return 1
''', json.dumps(rows), MAX_PENDING_BYTES))

    def groups(self):
        return list(self.db.smembers(self.key('groups')))

    def configuration(self, group):
        sid, cfg = json.loads(self.db.hget(self.key('destinations'), group))
        return sid, cfg

    def count(self, group):
        return self.db.xlen(self.key('queue:' + group))

    def events(self, group, limit, byte_limit):
        out, total = [], 0
        for seq, fields in self.db.xrange(self.key('queue:' + group), count=limit):
            size = int(fields['size'])
            if out and total + size > byte_limit: break
            out.append((seq, json.loads(fields['payload'])))
            total += size
        return out

    def batch(self, group):
        data = self.db.hgetall(self.key('batch:' + group))
        if not data: return None
        return data['id'], data['payload'].encode(), int(data['attempts']), float(data['next_attempt'])

    def prepare(self, group, events, mask):
        sid, _ = self.configuration(group)
        bid = str(uuid.uuid4())
        selected = [event for (_, event), keep in zip(events, mask) if keep]
        payload = json.dumps({'version':1, 'batch_id':bid, 'session_id':sid, 'events':selected}, separators=(',', ':'))
        self.write('''
local p=KEYS[1]; local g=ARGV[1]; local q=p..'queue:'..g; local b=p..'batch:'..g
if redis.call('EXISTS',b)==1 then return 0 end
local rows=cjson.decode(ARGV[4]); local ids={}
for _,r in ipairs(rows) do
 if #redis.call('XRANGE',q,r[1],r[1])==0 then return 0 end
end
for _,r in ipairs(rows) do
 if r[2] then table.insert(ids,r[1]) else
  local e=redis.call('XRANGE',q,r[1],r[1])[1][2]
  redis.call('DECRBY',p..'bytes',tonumber(e[4])); redis.call('XDEL',q,r[1])
 end
end
if #ids>0 then
 redis.call('HSET',b,'id',ARGV[2],'payload',ARGV[3],'attempts',0,'next_attempt',0,'ids',cjson.encode(ids))
 redis.call('HSET',p..'batches',ARGV[2],g)
elseif redis.call('XLEN',q)==0 then redis.call('SREM',p..'groups',g) end
return 1
''', group, bid, payload, json.dumps([[seq, keep] for (seq, _), keep in zip(events, mask)]))

    def confirm(self):
        # A previous prepare may have timed out after Redis applied it. Ensure
        # its persisted payload is fsynced before sending that recovered batch.
        self.write("return redis.call('INCR',KEYS[1]..'sync')")

    def retry(self, bid, attempts, next_attempt):
        self.write('''local p=KEYS[1]; local g=redis.call('HGET',p..'batches',ARGV[1])
if g then redis.call('HSET',p..'batch:'..g,'attempts',ARGV[2],'next_attempt',ARGV[3]) end
return 1''', bid, attempts, next_attempt)

    def acknowledge(self, bid):
        self.write('''local p=KEYS[1]; local g=redis.call('HGET',p..'batches',ARGV[1])
if not g then return 0 end
local q=p..'queue:'..g; local b=p..'batch:'..g
for _,id in ipairs(cjson.decode(redis.call('HGET',b,'ids'))) do
 local e=redis.call('XRANGE',q,id,id)
 if #e>0 then redis.call('DECRBY',p..'bytes',tonumber(e[1][2][4])); redis.call('XDEL',q,id) end
end
redis.call('DEL',b); redis.call('HDEL',p..'batches',ARGV[1])
if redis.call('XLEN',q)==0 then redis.call('SREM',p..'groups',g); redis.call('DEL',q) end
return 1''', bid)

    def close(self):
        self.db.close()
