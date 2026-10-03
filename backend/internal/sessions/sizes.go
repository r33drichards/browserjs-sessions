package sessions

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// Session sizes. Every session is the blueprint; a size is what the
// blueprint's pod is given to run in: each container's CPU and memory, the
// size of /dev/shm, and the settings inside the pod that go with them. The
// blueprint as written is the size "small", and the only one a warm pool
// holds. The others are in a file beside it (sizes.yaml), which can say
// nothing but those things: the sizes cannot drift apart in anything else.
//
// A session's size is on its Sandbox (AnnSize; none means small) and in its
// pod template. It is chosen at create and changed by Resize. A pod keeps
// the size it started with, and a Pod Snapshot is only restored into the pod
// spec it was taken from, so a new size takes effect at the session's next
// start and that start is a fresh one: its disk, not its memory.

const (
	// AnnSize is the size the session's pod template has: what it runs at,
	// or will when it next starts. Absent for small.
	AnnSize = "browserjs.dev/size"
	// AnnResizeTo is a size asked for while the session was awake. It is
	// put into the pod template when the session next starts.
	AnnResizeTo = "browserjs.dev/resize-to"

	// DefaultSize is the blueprint as it is written.
	DefaultSize = "small"
)

var (
	ErrInvalidSize = errors.New("no such size")
	// ErrNoCapacity is a session that would wait for a node that is not
	// coming: every session node is full and no more can be added.
	ErrNoCapacity = errors.New("no capacity")
)

// NoCapacityError is ErrNoCapacity for one size.
type NoCapacityError struct{ Size string }

func (e *NoCapacityError) Error() string {
	return fmt.Sprintf("no capacity for a %s session right now: every session node is full. Try again later, or pick a smaller size.", e.Size)
}
func (e *NoCapacityError) Unwrap() error { return ErrNoCapacity }

// InvalidSizeError is ErrInvalidSize, saying which sizes there are.
type InvalidSizeError struct {
	Size    string
	Offered []string
}

func (e *InvalidSizeError) Error() string {
	return fmt.Sprintf("size must be one of %q, got %q", e.Offered, e.Size)
}
func (e *InvalidSizeError) Unwrap() error { return ErrInvalidSize }

// SizesFile is sizes.yaml: the sizes other than small, and how much room
// the cluster has for sessions.
type SizesFile struct {
	Sizes    []SizeSpec    `json:"sizes"`
	Capacity *CapacitySpec `json:"capacity,omitempty"`
}

// SizeSpec is one size: all that it may change of the blueprint.
type SizeSpec struct {
	Name string `json:"name"`
	// By container name. A container not named keeps the blueprint's.
	Containers map[string]ContainerSize `json:"containers"`
	// The sizeLimit of the pod's "shm" volume (/dev/shm). It is memory, and
	// counts against the browser container's limit.
	Shm string `json:"shm,omitempty"`
}

type ContainerSize struct {
	Resources ResourceSpec `json:"resources"`
	// Environment variables this size sets. A variable a size sets is the
	// size's alone: it is removed from a session resized to a size that
	// does not set it.
	Env map[string]string `json:"env,omitempty"`
}

type ResourceSpec struct {
	Requests map[string]string `json:"requests"`
	Limits   map[string]string `json:"limits"`
}

// CapacitySpec is the room there is for session pods: at most Nodes session
// nodes, each with CPU and Memory left for sessions once the node's own pods
// have theirs. Without it nothing is refused for want of room.
type CapacitySpec struct {
	Nodes  int    `json:"nodes"`
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// SizeInfo is a size as a user chooses it: what the desktop (the browser
// container) may use, and what the whole pod reserves.
type SizeInfo struct {
	Name string `json:"name"`
	// The desktop's limits.
	CPUMillis int64 `json:"cpuMillis"`
	MemoryMiB int64 `json:"memoryMiB"`
	// Whether a new session of this size can come from the warm pool.
	Warm bool `json:"warm"`
}

// shape is a size, ready to be put into a pod template.
type shape struct {
	resources map[string]map[string]any    // by container; nil for none
	env       map[string]map[string]string // by container
	shm       string
}

type sizes struct {
	order    []string
	shapes   map[string]shape
	sizeEnv  map[string]map[string]bool // by container: every variable some size sets
	nodes    int                        // 0: no capacity check
	nodeCPU  int64                      // millicores
	nodeMem  int64                      // bytes
	desktops map[string]SizeInfo
}

var sizeName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// desktopContainer is the container a size's numbers are shown for.
const desktopContainer = "browser"

func quantity(s string) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return q, fmt.Errorf("%q is not a quantity: %w", s, err)
	}
	return q, nil
}

func (r ResourceSpec) raw() map[string]any {
	out := map[string]any{}
	for key, list := range map[string]map[string]string{"requests": r.Requests, "limits": r.Limits} {
		if len(list) == 0 {
			continue
		}
		m := map[string]any{}
		for k, v := range list {
			m[k] = v
		}
		out[key] = m
	}
	return out
}

// ParseSizes reads sizes.yaml. It refuses a file that names a size twice or
// "small", a request above its limit, or a quantity that is not one.
func ParseSizes(data []byte) (SizesFile, error) {
	var f SizesFile
	if err := yaml.UnmarshalStrict(data, &f); err != nil {
		return SizesFile{}, fmt.Errorf("sizes: %w", err)
	}
	seen := map[string]bool{DefaultSize: true}
	for _, size := range f.Sizes {
		if !sizeName.MatchString(size.Name) {
			return SizesFile{}, fmt.Errorf("sizes: %q is not a size's name", size.Name)
		}
		if seen[size.Name] {
			return SizesFile{}, fmt.Errorf("sizes: %q is there twice (small is the blueprint itself)", size.Name)
		}
		seen[size.Name] = true
		if len(size.Containers) == 0 {
			return SizesFile{}, fmt.Errorf("sizes: %s changes no container", size.Name)
		}
		for name, c := range size.Containers {
			for _, res := range []string{"cpu", "memory"} {
				req, lim := c.Resources.Requests[res], c.Resources.Limits[res]
				if req == "" || lim == "" {
					return SizesFile{}, fmt.Errorf("sizes: %s, %s: a %s request and limit are required", size.Name, name, res)
				}
				r, err := quantity(req)
				if err != nil {
					return SizesFile{}, fmt.Errorf("sizes: %s, %s: %w", size.Name, name, err)
				}
				l, err := quantity(lim)
				if err != nil {
					return SizesFile{}, fmt.Errorf("sizes: %s, %s: %w", size.Name, name, err)
				}
				if r.Cmp(l) > 0 {
					return SizesFile{}, fmt.Errorf("sizes: %s, %s: the %s request %s is above the limit %s", size.Name, name, res, req, lim)
				}
			}
		}
		if size.Shm != "" {
			if _, err := quantity(size.Shm); err != nil {
				return SizesFile{}, fmt.Errorf("sizes: %s, shm: %w", size.Name, err)
			}
		}
	}
	if c := f.Capacity; c != nil {
		if c.Nodes < 1 {
			return SizesFile{}, errors.New("sizes: capacity.nodes must be at least 1")
		}
		for _, q := range []string{c.CPU, c.Memory} {
			if _, err := quantity(q); err != nil {
				return SizesFile{}, fmt.Errorf("sizes: capacity: %w", err)
			}
		}
	}
	return f, nil
}

func podSpecOf(spec map[string]any) map[string]any {
	pod, _, _ := unstructured.NestedFieldNoCopy(spec, "podTemplate", "spec")
	m, _ := pod.(map[string]any)
	return m
}

func containersOf(spec map[string]any) []map[string]any {
	list, _ := podSpecOf(spec)["containers"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if c, ok := item.(map[string]any); ok {
			out = append(out, c)
		}
	}
	return out
}

func shmOf(spec map[string]any) map[string]any {
	list, _ := podSpecOf(spec)["volumes"].([]any)
	for _, item := range list {
		if v, ok := item.(map[string]any); ok && v["name"] == "shm" {
			dir, _ := v["emptyDir"].(map[string]any)
			return dir
		}
	}
	return nil
}

// newSizes makes the sizes of a blueprint: small from the blueprint's own
// Sandbox spec, the rest from file.
func newSizes(blueprint map[string]any, file SizesFile) (*sizes, error) {
	z := &sizes{shapes: map[string]shape{}, sizeEnv: map[string]map[string]bool{}, desktops: map[string]SizeInfo{}}
	small := shape{resources: map[string]map[string]any{}, env: map[string]map[string]string{}}
	names := map[string]bool{}
	for _, c := range containersOf(blueprint) {
		name, _ := c["name"].(string)
		names[name] = true
		res, _ := c["resources"].(map[string]any)
		small.resources[name] = res
	}
	if dir := shmOf(blueprint); dir != nil {
		small.shm, _ = dir["sizeLimit"].(string)
	}
	z.order, z.shapes[DefaultSize] = []string{DefaultSize}, small
	for _, size := range file.Sizes {
		sh := shape{resources: map[string]map[string]any{}, env: map[string]map[string]string{}, shm: size.Shm}
		if sh.shm == "" {
			sh.shm = small.shm
		} else if shmOf(blueprint) == nil {
			return nil, fmt.Errorf("sizes: %s sets shm, and the blueprint has no \"shm\" volume", size.Name)
		}
		for name := range names {
			sh.resources[name] = small.resources[name]
		}
		for name, c := range size.Containers {
			if !names[name] {
				return nil, fmt.Errorf("sizes: %s names container %q, which the blueprint does not have", size.Name, name)
			}
			sh.resources[name] = c.Resources.raw()
			sh.env[name] = c.Env
			for key := range c.Env {
				if z.sizeEnv[name] == nil {
					z.sizeEnv[name] = map[string]bool{}
				}
				z.sizeEnv[name][key] = true
			}
		}
		z.order, z.shapes[size.Name] = append(z.order, size.Name), sh
	}
	// A variable a size sets must be the sizes' alone: resizing removes it.
	for _, c := range containersOf(blueprint) {
		name, _ := c["name"].(string)
		env, _ := c["env"].([]any)
		for _, e := range env {
			if m, ok := e.(map[string]any); ok && z.sizeEnv[name][fmt.Sprint(m["name"])] {
				return nil, fmt.Errorf("sizes: %s is set by a size and by the blueprint's %s container", m["name"], name)
			}
		}
	}
	if c := file.Capacity; c != nil {
		cpu, _ := quantity(c.CPU)
		mem, _ := quantity(c.Memory)
		z.nodes, z.nodeCPU, z.nodeMem = c.Nodes, cpu.MilliValue(), mem.Value()
	}
	for _, name := range z.order {
		spec := map[string]any{"podTemplate": map[string]any{"spec": map[string]any{}}}
		list := []any{}
		for c, res := range z.shapes[name].resources {
			item := map[string]any{"name": c}
			if res != nil {
				item["resources"] = res
			}
			list = append(list, item)
		}
		podSpecOf(spec)["containers"] = list
		info := SizeInfo{Name: name, Warm: name == DefaultSize}
		if res := z.shapes[name].resources[desktopContainer]; res != nil {
			cpu, mem := quantities(res, "limits")
			info.CPUMillis, info.MemoryMiB = cpu, mem>>20
		}
		z.desktops[name] = info
		if cpu, mem := requestsOf(spec); z.nodes > 0 && (cpu > z.nodeCPU || mem > z.nodeMem) {
			return nil, fmt.Errorf("sizes: a %s session asks for more than a whole session node has (capacity)", name)
		}
	}
	return z, nil
}

// quantities reads cpu (millicores) and memory (bytes) of resources[which].
func quantities(resources map[string]any, which string) (cpu, mem int64) {
	list, _ := resources[which].(map[string]any)
	if q, err := resource.ParseQuantity(fmt.Sprint(list["cpu"])); err == nil && list["cpu"] != nil {
		cpu = q.MilliValue()
	}
	if q, err := resource.ParseQuantity(fmt.Sprint(list["memory"])); err == nil && list["memory"] != nil {
		mem = q.Value()
	}
	return cpu, mem
}

// requestsOf is what a Sandbox spec's pod asks the scheduler for.
func requestsOf(spec map[string]any) (cpu, mem int64) {
	for _, c := range containersOf(spec) {
		res, _ := c["resources"].(map[string]any)
		ccpu, cmem := quantities(res, "requests")
		cpu, mem = cpu+ccpu, mem+cmem
	}
	return cpu, mem
}

// apply puts a size into a Sandbox spec: resources, /dev/shm, and the
// sizes' environment variables. Nothing else is touched.
func (z *sizes) apply(spec map[string]any, size string) error {
	sh, ok := z.shapes[size]
	if !ok {
		return z.invalid(size)
	}
	for _, c := range containersOf(spec) {
		name, _ := c["name"].(string)
		res, known := sh.resources[name]
		if !known {
			continue
		}
		if res == nil {
			delete(c, "resources")
		} else {
			c["resources"] = runtimeCopy(res)
		}
		if len(z.sizeEnv[name]) == 0 {
			continue
		}
		env, _ := c["env"].([]any)
		kept := make([]any, 0, len(env))
		for _, e := range env {
			if m, ok := e.(map[string]any); ok && z.sizeEnv[name][fmt.Sprint(m["name"])] {
				continue
			}
			kept = append(kept, e)
		}
		keys := make([]string, 0, len(sh.env[name]))
		for key := range sh.env[name] {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			kept = append(kept, map[string]any{"name": key, "value": sh.env[name][key]})
		}
		if len(kept) == 0 {
			delete(c, "env")
		} else {
			c["env"] = kept
		}
	}
	if dir := shmOf(spec); dir != nil && sh.shm != "" {
		dir["sizeLimit"] = sh.shm
	}
	return nil
}

func runtimeCopy(m map[string]any) map[string]any {
	return (&unstructured.Unstructured{Object: m}).DeepCopy().Object
}

func (z *sizes) invalid(size string) error {
	return &InvalidSizeError{Size: size, Offered: append([]string(nil), z.order...)}
}

// EnableSizes offers the sizes of file beside small. Without it small is the
// only size, and nothing is refused for want of room.
func (s *Store) EnableSizes(file SizesFile) error {
	z, err := newSizes(s.small, file)
	if err != nil {
		return err
	}
	s.sizes = z
	return nil
}

// Sizes is the sizes a session can have, small first.
func (s *Store) Sizes() []SizeInfo {
	out := make([]SizeInfo, 0, len(s.sizes.order))
	for _, name := range s.sizes.order {
		out = append(out, s.sizes.desktops[name])
	}
	return out
}

// size checks a size asked for; "" is small.
func (s *Store) size(asked string) (string, error) {
	if asked == "" {
		return DefaultSize, nil
	}
	if _, ok := s.sizes.shapes[asked]; !ok {
		return "", s.sizes.invalid(asked)
	}
	return asked, nil
}

// sizeOf is the size a Sandbox's pod template has.
func sizeOf(obj *unstructured.Unstructured) string {
	if size := obj.GetAnnotations()[AnnSize]; size != "" {
		return size
	}
	return DefaultSize
}

func setSize(obj *unstructured.Unstructured, size string) {
	if size == DefaultSize {
		size = ""
	}
	setAnnotation(obj, AnnSize, size)
}

// applyResize puts a waiting resize into the Sandbox's pod template. It
// reports whether there was one. The Sandbox must be suspended: a pod
// template is only read when a pod is made. Its snapshot, of a pod of the
// old size, is forgotten.
func (s *Store) applyResize(obj *unstructured.Unstructured) (bool, error) {
	to := obj.GetAnnotations()[AnnResizeTo]
	if to == "" {
		return false, nil
	}
	setAnnotation(obj, AnnResizeTo, "")
	if to == sizeOf(obj) {
		return false, nil
	}
	if _, ok := s.sizes.shapes[to]; !ok {
		// A size that is no longer offered: the session stays as it is.
		return false, nil
	}
	spec, _ := obj.Object["spec"].(map[string]any)
	if err := s.sizes.apply(spec, to); err != nil {
		return false, err
	}
	setSize(obj, to)
	return true, setSnapshot(obj, nil)
}

// Resize changes a session's size. One that is asleep or stopped has it at
// once and starts at the new size; one that is awake keeps running as it is
// and has the new size from its next start (Session.PendingSize until then).
// Either way that start is a fresh one, from the session's disk: the state
// saved at a sleep is of a pod of the old size, and is dropped.
func (s *Store) Resize(ctx context.Context, id, asked string) error {
	size, err := s.size(asked)
	if err != nil {
		return err
	}
	applied := false
	err = s.modify(ctx, id, func(obj *unstructured.Unstructured) (bool, error) {
		applied = false
		if obj.GetDeletionTimestamp() != nil {
			return false, ErrNotFound
		}
		ann := obj.GetAnnotations()
		if size == sizeOf(obj) {
			// As it is: only a resize that was waiting is withdrawn.
			if ann[AnnResizeTo] == "" {
				return false, nil
			}
			setAnnotation(obj, AnnResizeTo, "")
			return true, nil
		}
		if operatingMode(obj) != "Suspended" && ann[AnnResizeTo] == size {
			return false, nil
		}
		setAnnotation(obj, AnnResizeTo, size)
		if operatingMode(obj) == "Suspended" {
			var err error
			applied, err = s.applyResize(obj)
			return true, err
		}
		return true, nil
	})
	if err == nil && applied && s.snap != nil {
		s.snap.pruneLogged(ctx, id, "")
	}
	return err
}

// room refuses (ErrNoCapacity) a pod asking for cpu and mem that no session
// node has room for and no new node could take. It is a look, not a
// reservation: the scheduler decides, and a session that loses a race waits
// as "starting". exclude is a Sandbox not to count (the one being started).
//
// What is on each node is read from the Sandboxes themselves: every session
// and every warm one is a Sandbox, with its node in its status and what its
// pod asks for in its template.
func (s *Store) room(ctx context.Context, size string, spec map[string]any, exclude string) error {
	z := s.sizes
	if z.nodes == 0 {
		return nil
	}
	cpu, mem := requestsOf(spec)
	list, err := s.client.List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	type use struct{ cpu, mem int64 }
	nodes := map[string]*use{}
	var waiting []use
	for i := range list.Items {
		obj := &list.Items[i]
		if obj.GetName() == exclude || obj.GetDeletionTimestamp() != nil || operatingMode(obj) == "Suspended" {
			continue
		}
		sp, _ := obj.Object["spec"].(map[string]any)
		c, m := requestsOf(sp)
		node, _, _ := unstructured.NestedString(obj.Object, "status", "nodeName")
		if node == "" {
			waiting = append(waiting, use{c, m})
			continue
		}
		if nodes[node] == nil {
			nodes[node] = &use{}
		}
		nodes[node].cpu += c
		nodes[node].mem += m
	}
	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	place := func(u use) bool {
		for _, name := range names {
			if n := nodes[name]; n.cpu+u.cpu <= z.nodeCPU && n.mem+u.mem <= z.nodeMem {
				n.cpu, n.mem = n.cpu+u.cpu, n.mem+u.mem
				return true
			}
		}
		if len(names) >= z.nodes || u.cpu > z.nodeCPU || u.mem > z.nodeMem {
			return false
		}
		name := fmt.Sprintf("\x00new-%d", len(names))
		names, nodes[name] = append(names, name), &use{u.cpu, u.mem}
		return true
	}
	// Pods already waiting for a node were there first.
	for _, u := range waiting {
		place(u)
	}
	if !place(use{cpu, mem}) {
		return &NoCapacityError{Size: size}
	}
	return nil
}

// roomToStart is room for a suspended Sandbox about to run again.
func (s *Store) roomToStart(ctx context.Context, obj *unstructured.Unstructured) error {
	spec, _ := obj.Object["spec"].(map[string]any)
	return s.room(ctx, sizeOf(obj), spec, obj.GetName())
}

// start prepares a suspended Sandbox to run again, for Wake and for a
// resume: a waiting resize is applied, and the start is refused when there
// is no room for it. It reports whether the size changed (the session's
// snapshots are then to be deleted).
func (s *Store) start(ctx context.Context, id string) (resized bool, err error) {
	err = s.modify(ctx, id, func(obj *unstructured.Unstructured) (bool, error) {
		resized = false
		if operatingMode(obj) != "Suspended" {
			return false, nil
		}
		var err error
		if resized, err = s.applyResize(obj); err != nil {
			return false, err
		}
		if err := s.roomToStart(ctx, obj); err != nil {
			return false, err
		}
		return resized, nil
	})
	if apierrors.IsNotFound(err) {
		return false, ErrNotFound
	}
	if err == nil && resized && s.snap != nil {
		s.snap.pruneLogged(ctx, id, "")
	}
	return resized, err
}
