"""A Kubernetes API server, as far as kopf and the operator use one:
discovery, list, watch, merge patches (with the status subresource and
finalizers), events. In memory, for tests.
"""
import asyncio
import copy
import json
from datetime import datetime, timezone

from aiohttp import web

RESOURCES = {
    ("browserjs.dev", "v1alpha1"): [("sessionpolicies", "SessionPolicy", True, True)],
    ("discovery.k8s.io", "v1"): [("endpointslices", "EndpointSlice", True, False)],
    ("apiextensions.k8s.io", "v1"): [("customresourcedefinitions", "CustomResourceDefinition", False, False)],
}
VERBS = ["get", "list", "watch", "create", "update", "patch", "delete"]


def merge(target, patch):
    """RFC 7386."""
    if not isinstance(patch, dict):
        return patch
    out = dict(target) if isinstance(target, dict) else {}
    for key, value in patch.items():
        if value is None:
            out.pop(key, None)
        else:
            out[key] = merge(out.get(key), value)
    return out


def json_patch(target, operations):
    """RFC 6902, as far as kopf uses it: test, add, replace, remove."""
    out = copy.deepcopy(target)
    for op in operations:
        keys = [k.replace("~1", "/").replace("~0", "~") for k in op["path"].split("/")[1:]]
        parent = out
        for key in keys[:-1]:
            parent = parent[int(key)] if isinstance(parent, list) else parent.setdefault(key, {})
        last = keys[-1]
        if isinstance(parent, list):
            last = len(parent) if last == "-" else int(last)
        if op["op"] == "test":
            current = parent[last] if isinstance(parent, list) else parent.get(last)
            if current != op["value"]:
                raise ValueError("test failed")
        elif op["op"] == "remove":
            del parent[last]
        elif op["op"] == "add" and isinstance(parent, list):
            parent.insert(last, op["value"])
        else:
            parent[last] = op["value"]
    return out


class FakeKube:
    def __init__(self):
        self.objects = {}   # (plural, name) -> object
        self.version = 0
        self.watchers = {}  # plural -> [queue]
        self.requests = []  # (method, path) of every write
        self.events = []

    # --- what a test does to the cluster ------------------------------------

    def _emit(self, kind, plural, obj):
        for queue in self.watchers.get(plural, []):
            queue.put_nowait({"type": kind, "object": copy.deepcopy(obj)})

    def put(self, plural, obj):
        """Create, or replace the spec of, an object."""
        name = obj["metadata"]["name"]
        old = self.objects.get((plural, name))
        self.version += 1
        new = copy.deepcopy(obj)
        if old:
            new["metadata"] = {**old["metadata"], **new["metadata"]}
            new.setdefault("status", old.get("status", {}))
            if new.get("spec") != old.get("spec"):
                new["metadata"]["generation"] = old["metadata"].get("generation", 1) + 1
        else:
            new["metadata"].setdefault("generation", 1)
            new["metadata"].setdefault("uid", f"uid-{name}")
            new["metadata"].setdefault("creationTimestamp", datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))
        new["metadata"]["resourceVersion"] = str(self.version)
        self.objects[(plural, name)] = new
        self._emit("MODIFIED" if old else "ADDED", plural, new)

    def delete(self, plural, name):
        obj = self.objects[(plural, name)]
        self.version += 1
        obj["metadata"]["resourceVersion"] = str(self.version)
        if obj["metadata"].get("finalizers"):
            obj["metadata"]["deletionTimestamp"] = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
            self._emit("MODIFIED", plural, obj)
        else:
            del self.objects[(plural, name)]
            self._emit("DELETED", plural, obj)

    def get(self, plural, name):
        return self.objects.get((plural, name))

    # --- HTTP ----------------------------------------------------------------

    async def api(self, request):
        return web.json_response({"kind": "APIVersions", "versions": ["v1"]})

    async def core(self, request):
        return web.json_response({"kind": "APIResourceList", "groupVersion": "v1", "resources": [
            {"name": "events", "singularName": "", "namespaced": True, "kind": "Event", "verbs": VERBS},
            {"name": "namespaces", "singularName": "", "namespaced": False, "kind": "Namespace", "verbs": VERBS}]})

    async def groups(self, request):
        return web.json_response({"kind": "APIGroupList", "groups": [
            {"name": g, "versions": [{"groupVersion": f"{g}/{v}", "version": v}],
             "preferredVersion": {"groupVersion": f"{g}/{v}", "version": v}} for g, v in RESOURCES]})

    async def group(self, request):
        key = (request.match_info["group"], request.match_info["version"])
        resources = []
        for plural, kind, namespaced, has_status in RESOURCES.get(key, []):
            resources.append({"name": plural, "singularName": kind.lower(), "namespaced": namespaced,
                              "kind": kind, "verbs": VERBS})
            if has_status:
                resources.append({"name": f"{plural}/status", "singularName": "", "namespaced": namespaced,
                                  "kind": kind, "verbs": ["get", "patch", "update"]})
        return web.json_response({"kind": "APIResourceList", "groupVersion": "/".join(key), "resources": resources})

    async def collection(self, request):
        plural = request.match_info["plural"]
        if request.query.get("watch") not in ("true", "1"):
            items = [copy.deepcopy(o) for (p, _), o in sorted(self.objects.items()) if p == plural]
            return web.json_response({"kind": "List", "apiVersion": "v1",
                                      "metadata": {"resourceVersion": str(self.version)}, "items": items})
        queue = asyncio.Queue()
        self.watchers.setdefault(plural, []).append(queue)
        response = web.StreamResponse(headers={"Content-Type": "application/json"})
        await response.prepare(request)
        try:
            # Changes made between the list and this watch.
            since = int(request.query.get("resourceVersion") or 0)
            for (p, _), o in sorted(self.objects.items()):
                if p == plural and int(o["metadata"]["resourceVersion"]) > since:
                    await response.write(json.dumps({"type": "ADDED", "object": o}).encode() + b"\n")
            while True:
                event = await queue.get()
                await response.write(json.dumps(event).encode() + b"\n")
        except (asyncio.CancelledError, ConnectionResetError):
            raise
        finally:
            self.watchers[plural].remove(queue)
        return response

    async def patch(self, request):
        plural, name = request.match_info["plural"], request.match_info["name"]
        status_only = request.match_info.get("sub") == "status"
        self.requests.append(("PATCH", request.path))
        obj = self.objects.get((plural, name))
        if obj is None:
            return web.json_response({"kind": "Status", "code": 404, "reason": "NotFound"}, status=404)
        body = await request.json()
        if isinstance(body, list):
            try:
                patched = json_patch(obj, body)
            except ValueError:
                return web.json_response({"kind": "Status", "code": 422, "reason": "Invalid"}, status=422)
        else:
            patched = merge(obj, body)
        # As an API server with the status subresource does: each endpoint
        # changes its own half and ignores the other.
        if status_only:
            new = dict(obj, status=patched.get("status") or {})
        else:
            new = dict(patched, status=obj.get("status") or {})
        self.version += 1
        new["metadata"]["resourceVersion"] = str(self.version)
        if new["metadata"].get("deletionTimestamp") and not new["metadata"].get("finalizers"):
            del self.objects[(plural, name)]
            self._emit("DELETED", plural, new)
        else:
            self.objects[(plural, name)] = new
            self._emit("MODIFIED", plural, new)
        return web.json_response(new)

    async def forbidden(self, request):
        # deploy.md gives the operator no rights on namespaces; kopf asks anyway.
        return web.json_response({"kind": "Status", "code": 403, "reason": "Forbidden",
                                  "message": "namespaces is forbidden"}, status=403)

    async def event(self, request):
        self.events.append(await request.json())
        return web.json_response({}, status=201)

    def app(self):
        app = web.Application()
        app.router.add_get("/api", self.api)
        app.router.add_get("/api/v1", self.core)
        app.router.add_get("/apis", self.groups)
        app.router.add_get("/apis/{group}/{version}", self.group)
        app.router.add_get("/api/v1/namespaces", self.forbidden)
        app.router.add_post("/api/v1/namespaces/{ns}/events", self.event)
        for prefix in ("/apis/{group}/{version}/namespaces/{ns}/{plural}", "/apis/{group}/{version}/{plural}"):
            app.router.add_get(prefix, self.collection)
            app.router.add_patch(prefix + "/{name}", self.patch)
            app.router.add_patch(prefix + "/{name}/{sub}", self.patch)
        return app
