"""An API server and a Metronome ingest endpoint over HTTP, as far as the
observer uses them: paged list of Sandboxes; get, create and update of a
Lease; POST /v1/ingest.
"""
import copy

from aiohttp import web


class FakeAPI:
    def __init__(self, sandboxes=(), page_size: int = 500):
        self.sandboxes = {s["metadata"]["name"]: s for s in sandboxes}
        self.leases = {}
        self.version = 0
        self.page_size = page_size
        self.requests = []       # (method, path, Authorization)
        self.conflicts = 0       # this many Lease writes answer 409 first

    def _seen(self, request):
        self.requests.append((request.method, request.path, request.headers.get("Authorization")))

    async def discovery(self, request):
        self._seen(request)
        if request.path == "/api":
            return web.json_response({"kind": "APIVersions", "versions": ["v1"]})
        if request.path == "/apis":
            return web.json_response({"kind": "APIGroupList", "groups": []})
        return web.json_response({"kind": "APIResourceList", "groupVersion": "v1", "resources": []})

    async def list_sandboxes(self, request):
        self._seen(request)
        items = [copy.deepcopy(s) for _, s in sorted(self.sandboxes.items())]
        start = int(request.query.get("continue") or 0)
        size = min(int(request.query.get("limit") or self.page_size), self.page_size)
        more = start + size < len(items)
        return web.json_response({"kind": "List", "items": items[start:start + size],
                                  "metadata": {"continue": str(start + size)} if more else {}})

    async def get_lease(self, request):
        self._seen(request)
        lease = self.leases.get(request.match_info["name"])
        if lease is None:
            return web.json_response({"kind": "Status", "code": 404}, status=404)
        return web.json_response(lease)

    def _write(self, body):
        self.version += 1
        body["metadata"]["resourceVersion"] = str(self.version)
        self.leases[body["metadata"]["name"]] = body
        return web.json_response(body, status=201 if self.version == 1 else 200)

    async def post_lease(self, request):
        self._seen(request)
        body = await request.json()
        if self.conflicts or body["metadata"]["name"] in self.leases:
            self.conflicts = max(0, self.conflicts - 1)
            return web.json_response({"kind": "Status", "code": 409}, status=409)
        return self._write(body)

    async def put_lease(self, request):
        self._seen(request)
        body = await request.json()
        current = self.leases.get(request.match_info["name"])
        if self.conflicts or current is None or body["metadata"].get("resourceVersion") != current["metadata"]["resourceVersion"]:
            self.conflicts = max(0, self.conflicts - 1)
            return web.json_response({"kind": "Status", "code": 409}, status=409)
        return self._write(body)

    def app(self):
        app = web.Application()
        for path in ("/api", "/apis", "/api/v1", "/version"):
            app.router.add_get(path, self.discovery)
        app.router.add_get("/apis/agents.x-k8s.io/v1beta1/namespaces/{ns}/sandboxes", self.list_sandboxes)
        leases = "/apis/coordination.k8s.io/v1/namespaces/{ns}/leases"
        app.router.add_post(leases, self.post_lease)
        app.router.add_get(leases + "/{name}", self.get_lease)
        app.router.add_put(leases + "/{name}", self.put_lease)
        return app


class FakeIngest:
    """POST /v1/ingest: keeps what it is sent, by transaction_id; answers
    `status` when it is set."""

    def __init__(self):
        self.events = {}
        self.requests = []   # (Authorization, [transaction_id])
        self.status = None

    async def ingest(self, request):
        body = await request.json()
        self.requests.append((request.headers.get("Authorization"), [e["transaction_id"] for e in body]))
        if self.status:
            return web.json_response({"message": "no"}, status=self.status)
        for e in body:
            self.events.setdefault(e["transaction_id"], e)
        return web.json_response({})

    def app(self):
        app = web.Application()
        app.router.add_post("/v1/ingest", self.ingest)
        return app
