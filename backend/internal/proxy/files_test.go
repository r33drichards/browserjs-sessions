package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

// The four routes of a session's files, on the app's host.
func fileRoutes(id string) []struct{ method, path string } {
	base := "/api/sessions/" + id + "/files"
	return []struct{ method, path string }{
		{"GET", base}, {"GET", base + "/a.txt"}, {"PUT", base + "/a.txt"}, {"DELETE", base + "/a.txt"},
	}
}

// podFiles makes the pod answer like the browser container's file server.
func (e *env) podFiles(list string) {
	e.respondWith(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/files":
			_, _ = io.WriteString(w, list)
		case r.Method == "PUT":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"name":"a.txt","size":5}`)
		case r.Method == "DELETE":
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = io.WriteString(w, "file-bytes")
		}
	})
}

// A session's files are its owner's (and an admin's), like its screen.
func TestFilesAreTheOwners(t *testing.T) {
	e := newEnv(t)
	e.podFiles(`[]`)
	for _, c := range fileRoutes(e.id) {
		for user, want := range map[string]bool{alice: true, root: true, bob: false, "": false} {
			before := len(e.seen())
			rec := e.app(c.method, c.path, user, "bytes")
			switch {
			case want && rec.Code/100 != 2:
				t.Errorf("%s %s as %s: %d, want success", c.method, c.path, user, rec.Code)
			case !want && user == "" && rec.Code != http.StatusUnauthorized:
				t.Errorf("%s %s signed out: %d, want 401", c.method, c.path, rec.Code)
			case !want && user != "" && rec.Code != http.StatusNotFound:
				t.Errorf("%s %s as %s: %d, want 404 (as if there were no such session)", c.method, c.path, user, rec.Code)
			}
			if n := len(e.seen()) - before; !want && n != 0 {
				t.Errorf("%s %s as %q reached the pod", c.method, c.path, user)
			}
		}
	}
	// No such session, and not an ID at all.
	for _, id := range []string{"s-aaaaaaaaaa", "nope"} {
		for _, c := range fileRoutes(id) {
			if rec := e.app(c.method, c.path, alice, ""); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s: %d, want 404", c.method, c.path, rec.Code)
			}
		}
	}
}

// The files are on the app's host only: a session's host, which has routes
// without a login, has none of them.
func TestFilesAreNotOnTheSessionHost(t *testing.T) {
	e := newEnv(t)
	for _, c := range fileRoutes(e.id) {
		if rec := e.do(c.method, c.path, alice, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s on the session's host: %d, want 404", c.method, c.path, rec.Code)
		}
	}
	for _, path := range []string{"/files", "/files/a.txt"} {
		if rec := e.do("GET", path, alice, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s on the session's host: %d, want 404", path, rec.Code)
		}
	}
	if n := len(e.seen()); n != 0 {
		t.Errorf("%d of them reached the pod", n)
	}
}

// A name is one file of the folder. Nothing that could name another path of
// the pod, in any spelling, is sent to it.
func TestFileNamesCannotLeaveTheFolder(t *testing.T) {
	e := newEnv(t)
	e.podFiles(`[]`)
	base := "/api/sessions/" + e.id + "/files/"
	for _, name := range []string{
		"..", ".", "%2e%2e", "..%2Fmcp", "%2E%2E%2F%2E%2E%2Fetc%2Fpasswd", "a%2Fb", "a%5Cb", "..%5Cb",
		".hidden", ".upload-1", "a%00b", "a%0Ab", "a%7Fb", "%FF", "a/b", "a/../b", strings.Repeat("a", 256),
	} {
		for _, method := range []string{"GET", "PUT", "DELETE"} {
			rec := e.app(method, base+name, alice, "bytes")
			if rec.Code/100 == 2 {
				t.Errorf("%s %q: %d, want a refusal", method, name, rec.Code)
			}
		}
	}
	if calls := e.seen(); len(calls) != 0 {
		t.Errorf("a refused name reached the pod: %s %s", calls[0].method, calls[0].uri)
	}

	// An ordinary name, whatever is in it, is one escaped segment under /files/.
	for name, uri := range map[string]string{
		"report.pdf":          "/files/report.pdf",
		"my%20file%20(1).txt": "/files/my%20file%20%281%29.txt",
		"a%3Fb%23c%25d":       "/files/a%3Fb%23c%25d",
		"caf%C3%A9..txt":      "/files/caf%C3%A9..txt",
	} {
		rec := e.app("GET", base+name, alice, "")
		calls := e.seen()
		got := calls[len(calls)-1]
		if rec.Code != http.StatusOK || !strings.EqualFold(got.uri, uri) || strings.Count(got.path, "/") != 2 {
			t.Errorf("GET %q: %d, pod was asked for %q, want %q", name, rec.Code, got.uri, uri)
		}
	}
}

// The pod sees the browser container's own host name and nothing of the
// caller.
func TestFileRequestsCarryNothingOfTheCaller(t *testing.T) {
	e := newEnv(t)
	e.podFiles(`[]`)
	for _, c := range fileRoutes(e.id) {
		req := request(c.method, appHost, c.path, alice, "bytes")
		req.Header.Set("Cookie", "_pomerium=secret")
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("Range", "bytes=0-1")
		e.handler.ServeHTTP(httptest.NewRecorder(), req)
		calls := e.seen()
		got := calls[len(calls)-1]
		if got.host != "localhost:8081" {
			t.Errorf("%s %s: pod saw Host %q, want localhost:8081", c.method, c.path, got.host)
		}
		for _, h := range []string{"Cookie", "Authorization", "X-Pomerium-Jwt-Assertion", "Range"} {
			if h == "Range" && c.method != "GET" {
				continue
			}
			if v := got.header.Get(h); v != "" {
				t.Errorf("%s %s: pod saw %s: %q", c.method, c.path, h, v)
			}
		}
	}
}

// A file is whatever a website sent the session, and it is served on the
// app's own origin: it must only ever be saved, never shown.
func TestDownloadIsAlwaysAnAttachment(t *testing.T) {
	e := newEnv(t)
	e.respondWith(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Disposition", "inline")
		w.Header().Add("Set-Cookie", "evil=1; Path=/")
		w.Header().Set("Location", "https://evil.example")
		w.Header().Set("Refresh", "0; url=https://evil.example")
		w.Header().Set("Link", "<https://evil.example>; rel=preload")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_, _ = io.WriteString(w, "<script>alert(1)</script>")
	})
	rec := e.app("GET", "/api/sessions/"+e.id+"/files/evil%22%20page%C3%A9.html", alice, "")
	if rec.Code != http.StatusOK || rec.Body.String() != "<script>alert(1)</script>" {
		t.Fatalf("download: %d %q", rec.Code, rec.Body)
	}
	h := rec.Header()
	for name, want := range map[string]string{
		"Content-Type":                 "application/octet-stream",
		"X-Content-Type-Options":       "nosniff",
		"Content-Security-Policy":      "sandbox; default-src 'none'",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Cache-Control":                "no-store",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	cd := h.Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment;") || strings.Contains(cd, `"`+"evil\"") {
		t.Errorf("Content-Disposition = %q, want an attachment with the name safely quoted", cd)
	}
	for _, name := range []string{"Set-Cookie", "Location", "Refresh", "Link", "Access-Control-Allow-Origin"} {
		if v := h.Values(name); len(v) != 0 {
			t.Errorf("the pod's %s reached the browser: %q", name, v)
		}
	}

	// What is not a file is a plain message, not the pod's page either.
	e.respondWith(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "<b>no</b>")
	})
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		rec = e.app(method, "/api/sessions/"+e.id+"/files/gone.txt", alice, "x")
		if rec.Code != http.StatusNotFound || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") ||
			rec.Header().Get("Content-Disposition") != "" {
			t.Errorf("%s of a missing file: %d %v", method, rec.Code, rec.Header())
		}
	}
}

// A file larger than the limit is refused, declared or not, without being
// read to its end.
func TestOversizedFileIsRefused(t *testing.T) {
	e := newEnv(t)
	e.proxy.MaxFileBytes = 1024
	e.podFiles(`[]`)
	path := "/api/sessions/" + e.id + "/files/big.bin"

	if rec := e.app("PUT", path, alice, strings.Repeat("x", 1025)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("declared oversized file: %d, want 413", rec.Code)
	}
	if n := len(e.seen()); n != 0 {
		t.Errorf("a file refused by its declared length reached the pod (%d calls)", n)
	}

	front := httptest.NewServer(e.handler)
	defer front.Close()
	body := &endless{}
	req, err := http.NewRequestWithContext(t.Context(), "PUT", front.URL+path, io.NopCloser(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = appHost
	req.Header.Set("X-Pomerium-Jwt-Assertion", alice)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("chunked oversized file: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("chunked oversized file: %d, want 413", resp.StatusCode)
	}
	for _, c := range e.seen() {
		if len(c.body) > 1024 {
			t.Errorf("the pod was sent %d bytes of a 1024-byte limit", len(c.body))
		}
	}

	rec := e.app("PUT", path, alice, strings.Repeat("x", 1024))
	if rec.Code != http.StatusCreated || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("file at the limit: %d %q, want the pod's 201 as JSON", rec.Code, rec.Header().Get("Content-Type"))
	}
	calls := e.seen()
	if got := calls[len(calls)-1]; got.method != "PUT" || got.path != "/files/big.bin" || len(got.body) != 1024 {
		t.Errorf("pod got %s %s with %d bytes", got.method, got.path, len(got.body))
	}
}

// The list is read and written out again; the pod's own bytes never reach
// the page.
func TestFileListIsNotThePodsWord(t *testing.T) {
	e := newEnv(t)
	e.proxy.MaxFileBytes = 2048
	e.podFiles(`[{"name":"a.txt","size":5,"modified":"2026-10-01T00:00:00.000Z","extra":"<script>"},
		{"name":"../escape","size":1,"modified":""},{"name":".hidden","size":1,"modified":""},{"name":"neg","size":-1,"modified":""}]`)
	rec := e.app("GET", "/api/sessions/"+e.id+"/files", alice, "")
	var got struct {
		Files    []fileInfo `json:"files"`
		MaxBytes int64      `json:"max_bytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("list: %d %q", rec.Code, rec.Body)
	}
	if len(got.Files) != 1 || got.Files[0] != (fileInfo{"a.txt", 5, "2026-10-01T00:00:00.000Z"}) || got.MaxBytes != 2048 {
		t.Errorf("list = %+v", got)
	}
	if strings.Contains(rec.Body.String(), "script") || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("list handed on the pod's own bytes: %q (%s)", rec.Body, rec.Header().Get("Content-Type"))
	}

	// Not a list at all, or far too much of one.
	for _, body := range []string{"<html>", `{"files":[]}`, "[" + strings.Repeat(`{"name":"a","size":1,"modified":""},`, maxFileListBytes/30)} {
		e.podFiles(body)
		if rec := e.app("GET", "/api/sessions/"+e.id+"/files", alice, ""); rec.Code != http.StatusBadGateway {
			t.Errorf("list of %.20q…: %d, want 502", body, rec.Code)
		}
	}
	// A browser image from before it served files answers 404.
	e.respondWith(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if rec := e.app("GET", "/api/sessions/"+e.id+"/files", alice, ""); rec.Code != http.StatusNotImplemented {
		t.Errorf("list from an old image: %d, want 501", rec.Code)
	}
}

// The page lists the folder for as long as it is open: that neither wakes a
// sleeping session nor keeps one awake. Moving a file does both.
func TestListingFilesIsNotUseOfTheSession(t *testing.T) {
	e := newEnv(t)
	e.podFiles(`[]`)
	list := "/api/sessions/" + e.id + "/files"

	e.tracker.Idle([]string{e.id})
	e.skew.Add(int64(16 * time.Minute))
	if rec := e.app("GET", list, alice, ""); rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	if !isIdle(e.tracker, e.id) {
		t.Error("listing files was recorded as activity")
	}
	if _ = e.app("GET", list+"/a.txt", alice, ""); isIdle(e.tracker, e.id) {
		t.Error("a download was not recorded as activity")
	}

	e.asleep(t)
	if rec := e.app("GET", list, alice, ""); rec.Code != http.StatusConflict {
		t.Errorf("list of a sleeping session: %d, want 409", rec.Code)
	}
	if s, _ := e.store.Get(t.Context(), e.id); s.State != sessions.Asleep {
		t.Errorf("state after listing = %s, want asleep", s.State)
	}

	_ = e.app("PUT", list+"/a.txt", alice, "bytes")
	if s, _ := e.store.Get(t.Context(), e.id); s.State == sessions.Asleep {
		t.Error("an upload did not wake the session")
	}
}

// copy asks for names to be put on the session's clipboard, as the page does.
func (e *env) copy(user, contentType, body string) *httptest.ResponseRecorder {
	req := request("POST", appHost, "/api/sessions/"+e.id+"/clipboard", user, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Cookie", "_pomerium=secret")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// Putting files on a session's clipboard is its owner's, like the files.
func TestClipboardIsTheOwners(t *testing.T) {
	e := newEnv(t)
	e.respondWith(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	const body = `{"files":["a.png","b c.txt"],"extra":"x"}`
	for user, want := range map[string]int{alice: http.StatusNoContent, root: http.StatusNoContent, bob: http.StatusNotFound, "": http.StatusUnauthorized} {
		before := len(e.seen())
		if rec := e.copy(user, "application/json", body); rec.Code != want {
			t.Errorf("copy as %q: %d, want %d", user, rec.Code, want)
		}
		if n := len(e.seen()) - before; (want == http.StatusNoContent) != (n == 1) {
			t.Errorf("copy as %q: %d requests reached the pod", user, n)
		}
	}
	// What the pod is sent is written anew, as the pod's own host, with
	// nothing of the caller.
	calls := e.seen()
	got := calls[len(calls)-1]
	if got.method != "POST" || got.uri != "/clipboard" || got.host != "localhost:8081" || got.body != `{"files":["a.png","b c.txt"]}` ||
		got.header.Get("Content-Type") != "application/json" {
		t.Errorf("pod got %s %s (Host %s, %s): %s", got.method, got.uri, got.host, got.header.Get("Content-Type"), got.body)
	}
	for _, h := range []string{"Cookie", "Authorization", "X-Pomerium-Jwt-Assertion", "Origin"} {
		if v := got.header.Get(h); v != "" {
			t.Errorf("pod saw %s: %q", h, v)
		}
	}
	// Not on the session's own host, and no such session.
	if rec := e.do("POST", "/api/sessions/"+e.id+"/clipboard", alice, body); rec.Code != http.StatusNotFound {
		t.Errorf("copy on the session's host: %d, want 404", rec.Code)
	}
	if rec := e.app("POST", "/api/sessions/s-aaaaaaaaaa/clipboard", alice, body); rec.Code != http.StatusNotFound {
		t.Errorf("copy for no session: %d, want 404", rec.Code)
	}
}

// Only names of files of the folder are passed on, and only as JSON, which
// a page of another site cannot post without asking.
func TestClipboardTakesOnlyFileNames(t *testing.T) {
	e := newEnv(t)
	e.respondWith(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, body := range []string{
		`{"files":["../secret"]}`, `{"files":["a.png",".."]}`, `{"files":["/etc/passwd"]}`, `{"files":["a/b"]}`, `{"files":["a\\b"]}`,
		`{"files":[".hidden"]}`, `{"files":["a\u0000b"]}`, `{"files":[""]}`, `{"files":["` + strings.Repeat("a", 256) + `"]}`,
		`{"files":[]}`, `{}`, `{"files":"a.png"}`, `{"files":[1]}`, `[]`, `nonsense`, ``,
		`{"files":[` + strings.Repeat(`"a",`, maxClipboardFiles) + `"a"]}`,
		`{"files":["a"],"pad":"` + strings.Repeat("x", maxClipboardBytes) + `"}`,
	} {
		if rec := e.copy(alice, "application/json", body); rec.Code != http.StatusBadRequest {
			t.Errorf("copy of %.40q: %d, want 400", body, rec.Code)
		}
	}
	for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
		if rec := e.copy(alice, contentType, `{"files":["a.png"]}`); rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("copy as %q: %d, want 415", contentType, rec.Code)
		}
	}
	if n := len(e.seen()); n != 0 {
		t.Errorf("%d refused requests reached the pod", n)
	}
	if rec := e.copy(alice, "application/json; charset=utf-8", `{"files":["café (1).png"]}`); rec.Code != http.StatusNoContent {
		t.Errorf("an ordinary name: %d, want 204", rec.Code)
	}
}

// Only the pod's status is handed on, as the backend's own words.
func TestClipboardAnswersAreTheBackends(t *testing.T) {
	e := newEnv(t)
	for pod, want := range map[int]int{
		http.StatusNotFound:            http.StatusNotImplemented, // an image without the route
		http.StatusGone:                http.StatusNotFound,
		http.StatusServiceUnavailable:  http.StatusServiceUnavailable,
		http.StatusForbidden:           http.StatusBadGateway,
		http.StatusInternalServerError: http.StatusBadGateway,
	} {
		e.respondWith(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Add("Set-Cookie", "evil=1")
			w.WriteHeader(pod)
			_, _ = io.WriteString(w, "<script>alert(1)</script>")
		})
		rec := e.copy(alice, "application/json", `{"files":["a.png"]}`)
		if rec.Code != want || rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Set-Cookie") != "" ||
			strings.Contains(rec.Body.String(), "script") {
			t.Errorf("pod %d: %d %v %q, want %d as the backend's JSON", pod, rec.Code, rec.Header(), rec.Body, want)
		}
	}
	// It is use of the session: it wakes one that sleeps.
	e.respondWith(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	e.asleep(t)
	_ = e.copy(alice, "application/json", `{"files":["a.png"]}`)
	if s, _ := e.store.Get(t.Context(), e.id); s.State == sessions.Asleep {
		t.Error("a copy did not wake the session")
	}
}
