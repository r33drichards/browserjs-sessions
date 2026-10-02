package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// The browser container's own server. Of what it serves, only /files is
// handed on: the folder Chromium downloads to and its file chooser opens in.
const browserPort = 8081

// DefaultMaxFileBytes is the largest file sent to a session's folder.
const DefaultMaxFileBytes = 100 << 20

// The most of a pod's file list that is read.
const maxFileListBytes = 1 << 20

type fileInfo struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
}

// registerFiles adds the routes of a session's files. Like the VNC ticket
// they are on the app's host, behind auth.Middleware, where the browser's
// sign-in with the app reaches: a file is saved by following a link.
func (p *Proxy) registerFiles(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sessions/{id}/files", p.listFiles)
	mux.HandleFunc("GET /api/sessions/{id}/files/{name}", p.downloadFile)
	mux.HandleFunc("PUT /api/sessions/{id}/files/{name}", p.uploadFile)
	mux.HandleFunc("DELETE /api/sessions/{id}/files/{name}", p.deleteFile)
}

// validFileName reports whether name is one file of the folder and nothing
// else: a single path segment that is not hidden. The pod checks the same;
// neither relies on the other.
func validFileName(name string) bool {
	if name == "" || len(name) > 255 || !utf8.ValidString(name) || strings.HasPrefix(name, ".") {
		return false
	}
	for _, r := range name {
		if r == '/' || r == '\\' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func fileError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// fileSession is the session a files request is for, once the caller is
// known to be allowed it. It answers the request itself otherwise.
func (p *Proxy) fileSession(w http.ResponseWriter, r *http.Request) (string, bool) {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return "", false
	}
	id := r.PathValue("id")
	if !sessions.ValidID(id) {
		http.Error(w, "session not found", http.StatusNotFound)
		return "", false
	}
	if !p.allowed(w, r, u, id) {
		return "", false
	}
	return id, true
}

// fileRequest is fileSession for the routes that name a file: the pod path
// of that file as well. The name arrives unescaped, so an encoded "../" or
// separator is seen for what it is and refused.
func (p *Proxy) fileRequest(w http.ResponseWriter, r *http.Request) (id, path string, ok bool) {
	if id, ok = p.fileSession(w, r); !ok {
		return "", "", false
	}
	name := r.PathValue("name")
	if !validFileName(name) {
		fileError(w, http.StatusBadRequest, "not a file name")
		return "", "", false
	}
	return id, "/files/" + name, true
}

// listFiles lists the session's folder. The page asks again and again while
// it is open, so this is not use of the session: it neither wakes it nor
// keeps it awake.
func (p *Proxy) listFiles(w http.ResponseWriter, r *http.Request) {
	id, ok := p.fileSession(w, r)
	if !ok {
		return
	}
	s, err := p.Waker.Running(r.Context(), id)
	if errors.Is(err, ErrNotRunning) {
		fileError(w, http.StatusConflict, "session is asleep")
		return
	}
	if err != nil {
		lookupFailed(w, r, id, err)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://"+p.Target(s, browserPort)+"/files", nil)
	if err != nil {
		fileError(w, http.StatusBadGateway, "session is not responding")
		return
	}
	req.Host = "localhost:" + strconv.Itoa(browserPort)
	resp, err := p.quick.RoundTrip(req)
	if err != nil {
		p.Waker.Invalidate(id)
		if r.Context().Err() == nil {
			slog.Error("session pod did not answer", "session", id, "port", browserPort, "err", err)
		}
		fileError(w, http.StatusBadGateway, "session is not responding")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// A browser image from before it served files.
		fileError(w, http.StatusNotImplemented, "this session was created before file transfer existed; a new session has it")
		return
	}
	// What the pod answers is untrusted: it is read as a list of files and
	// written out again, never handed on as it came.
	var listed []fileInfo
	if resp.StatusCode != http.StatusOK ||
		json.NewDecoder(io.LimitReader(resp.Body, maxFileListBytes)).Decode(&listed) != nil {
		fileError(w, http.StatusBadGateway, "the session did not list its files")
		return
	}
	files := make([]fileInfo, 0, len(listed))
	for _, f := range listed {
		if validFileName(f.Name) && f.Size >= 0 {
			files = append(files, f)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"files": files, "max_bytes": p.MaxFileBytes})
}

// plainAnswer makes what the pod said about a file a plain message: only
// its status and text are kept.
func plainAnswer(resp *http.Response) {
	length := resp.Header.Get("Content-Length")
	clear(resp.Header)
	if length != "" {
		resp.Header.Set("Content-Length", length)
	}
	resp.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp.Header.Set("Cache-Control", "no-store")
}

// downloadFile sends one file of the folder. It is served on the app's own
// origin and its bytes are anything a website chose to send the session, so
// whatever the pod says about them it is a download and never a page: an
// attachment of no particular type, on top of what neuter does.
func (p *Proxy) downloadFile(w http.ResponseWriter, r *http.Request) {
	id, path, ok := p.fileRequest(w, r)
	if !ok {
		return
	}
	done := p.Idle.Open(id)
	defer done()
	s, err := p.Waker.EnsureAwake(r.Context(), id)
	if err != nil {
		lookupFailed(w, r, id, err)
		return
	}
	name := r.PathValue("name")
	// The pod sends whole files only.
	r.Header.Del("Range")
	r.Header.Del("If-Range")
	p.forwardWith(w, r, s, browserPort, path, p.quick, func(resp *http.Response) {
		plainAnswer(resp)
		if resp.StatusCode == http.StatusOK {
			resp.Header.Set("Content-Type", "application/octet-stream")
			resp.Header.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		}
	})
}

// uploadFile puts a file into the folder, of at most MaxFileBytes. The pod
// answers with the name it gave it: an existing file is not replaced.
func (p *Proxy) uploadFile(w http.ResponseWriter, r *http.Request) {
	id, path, ok := p.fileRequest(w, r)
	if !ok {
		return
	}
	if r.ContentLength > p.MaxFileBytes {
		fileError(w, http.StatusRequestEntityTooLarge, "file too large")
		return
	}
	done := p.Idle.Open(id)
	defer done()
	s, err := p.Waker.EnsureAwake(r.Context(), id)
	if err != nil {
		lookupFailed(w, r, id, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, p.MaxFileBytes)
	p.forwardWith(w, r, s, browserPort, path, p.quick, func(resp *http.Response) {
		plainAnswer(resp)
		if resp.StatusCode/100 == 2 {
			resp.Header.Set("Content-Type", "application/json")
		}
	})
}

func (p *Proxy) deleteFile(w http.ResponseWriter, r *http.Request) {
	id, path, ok := p.fileRequest(w, r)
	if !ok {
		return
	}
	done := p.Idle.Open(id)
	defer done()
	s, err := p.Waker.EnsureAwake(r.Context(), id)
	if err != nil {
		lookupFailed(w, r, id, err)
		return
	}
	p.forwardWith(w, r, s, browserPort, path, p.quick, plainAnswer)
}
