package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const maxJSON = 1 << 20

type user struct {
	ID, Email, Password string
	Created             time.Time
}
type session struct {
	UserID  string
	Expires time.Time
}
type device struct {
	ID, UserID, Name, Platform string
	Created                    time.Time
}
type upload struct {
	ID, UserID, DeviceID, FilePath, SHA256, Temp string
	Size                                         int64
	Received                                     bool
	Created                                      time.Time
}
type version struct {
	ID, UserID, FilePath, Blob, SHA256, DeviceID string
	Size                                         int64
	Created                                      time.Time
	Metadata                                     map[string]string
}
type state struct {
	Users    map[string]user
	Sessions map[string]session
	Devices  map[string]device
	Uploads  map[string]upload
	Versions map[string][]version
}

type store struct {
	mu   sync.Mutex
	file string
	data state
}
type app struct {
	db                              *store
	dataDir                         string
	blobs                           blobStore
	allowRegistration, secureCookie bool
	maxUpload                       int64
}

func newStore(file string) (*store, error) {
	s := &store{file: file, data: state{Users: map[string]user{}, Sessions: map[string]session{}, Devices: map[string]device{}, Uploads: map[string]upload{}, Versions: map[string][]version{}}}
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, fmt.Errorf("read metadata: %w", err)
	}
	return s, nil
}
func (s *store) save() error {
	b, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	tmp := s.file + ".tmp"
	if err = os.WriteFile(tmp, b, 0600); err == nil {
		err = os.Rename(tmp, s.file)
	}
	return err
}
func id() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func sha(b []byte) string { x := sha256.Sum256(b); return hex.EncodeToString(x[:]) }

func main() {
	dataDir := env("DATA_DIR", "./data")
	if err := os.MkdirAll(filepath.Join(dataDir, "staging"), 0700); err != nil {
		log.Fatal(err)
	}
	db, err := newStore(filepath.Join(dataDir, "metadata.json"))
	if err != nil {
		log.Fatal(err)
	}
	max, err := strconv.ParseInt(env("MAX_UPLOAD_BYTES", "2147483648"), 10, 64)
	if err != nil || max < 1 {
		log.Fatal("MAX_UPLOAD_BYTES must be a positive integer")
	}
	var blobs blobStore
	if env("STORAGE_BACKEND", "filesystem") == "s3" {
		blobs, err = newMinioBlobStore(context.Background(), env("S3_ENDPOINT", ""), env("S3_ACCESS_KEY", ""), env("S3_SECRET_KEY", ""), env("S3_BUCKET", ""), env("S3_USE_SSL", "false") == "true")
	} else if env("STORAGE_BACKEND", "filesystem") == "filebin" {
		blobs, err = newFileBinBlobStore(env("FILEBIN_URL", ""), env("FILEBIN_ID", ""), env("FILEBIN_PASSWORD", ""))
	} else if env("STORAGE_BACKEND", "filesystem") == "filesystem" {
		blobs, err = newFilesystemBlobStore(filepath.Join(dataDir, "blobs"))
	} else {
		log.Fatal("STORAGE_BACKEND must be filesystem, s3, or filebin")
	}
	if err != nil {
		log.Fatal(err)
	}
	a := &app{db: db, dataDir: dataDir, blobs: blobs, allowRegistration: env("ALLOW_REGISTRATION", "false") == "true", secureCookie: env("SECURE_COOKIES", "false") == "true", maxUpload: max}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", a.ui)
	mux.HandleFunc("/api/register", a.register)
	mux.HandleFunc("/api/login", a.login)
	mux.HandleFunc("/api/logout", a.auth(a.logout))
	mux.HandleFunc("/api/me", a.auth(a.me))
	mux.HandleFunc("/api/devices", a.auth(a.devices))
	mux.HandleFunc("/api/uploads/prepare", a.auth(a.prepare))
	mux.HandleFunc("/api/uploads/", a.auth(a.uploadRoute))
	mux.HandleFunc("/api/files", a.auth(a.files))
	mux.HandleFunc("/api/history", a.auth(a.history))
	mux.HandleFunc("/api/verify", a.auth(a.verify))
	mux.HandleFunc("/api/download", a.auth(a.download))
	server := &http.Server{Addr: env("LISTEN_ADDR", ":8080"), Handler: securityHeaders(mux), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	log.Printf("storage-reader listening on %s with data in %s", server.Addr, dataDir)
	log.Fatal(server.ListenAndServe())
}
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}
func (a *app) auth(next func(http.ResponseWriter, *http.Request, user)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == r.Header.Get("Authorization") {
			if c, e := r.Cookie("session"); e == nil {
				token = c.Value
			}
		}
		a.db.mu.Lock()
		s, ok := a.db.data.Sessions[token]
		u, exists := a.db.data.Users[s.UserID]
		if ok && time.Now().After(s.Expires) {
			delete(a.db.data.Sessions, token)
			_ = a.db.save()
			ok = false
		}
		a.db.mu.Unlock()
		if !ok || !exists {
			fail(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, r, u)
	}
}
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSON)
	defer r.Body.Close()
	if json.NewDecoder(r.Body).Decode(dst) != nil {
		fail(w, 400, "invalid JSON")
		return false
	}
	return true
}
func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, code int, msg string) {
	replyStatus(w, code, map[string]string{"error": msg})
}
func replyStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
func cleanPath(p string) (string, error) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\x00") {
		return "", errors.New("invalid path")
	}
	p = path.Clean(p)
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return "", errors.New("invalid path")
	}
	return p, nil
}
func fileKey(userID, p string) string { return userID + "\x00" + p }
func validEmail(e string) bool {
	return len(e) <= 254 && strings.Count(e, "@") == 1 && !strings.ContainsAny(e, " \t\r\n")
}

func (a *app) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		fail(w, 405, "POST required")
		return
	}
	var in struct{ Email, Password string }
	if !decode(w, r, &in) {
		return
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	// bcrypt only accepts the first 72 bytes; reject longer passwords rather
	// than silently weakening them or returning a generic hashing failure.
	if !validEmail(in.Email) || len(in.Password) < 12 || len(in.Password) > 72 {
		fail(w, 400, "email is invalid or password must be 12 to 72 bytes")
		return
	}
	a.db.mu.Lock()
	defer a.db.mu.Unlock()
	if len(a.db.data.Users) > 0 && !a.allowRegistration {
		fail(w, 403, "registration is disabled")
		return
	}
	for _, u := range a.db.data.Users {
		if u.Email == in.Email {
			fail(w, 409, "email already registered")
			return
		}
	}
	h, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		fail(w, 500, "password processing failed")
		return
	}
	u := user{ID: id(), Email: in.Email, Password: string(h), Created: time.Now().UTC()}
	a.db.data.Users[u.ID] = u
	if err := a.db.save(); err != nil {
		fail(w, 500, "could not save user")
		return
	}
	replyStatus(w, 201, map[string]any{"id": u.ID, "email": u.Email})
}
func (a *app) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		fail(w, 405, "POST required")
		return
	}
	var in struct{ Email, Password string }
	if !decode(w, r, &in) {
		return
	}
	a.db.mu.Lock()
	defer a.db.mu.Unlock()
	var u user
	found := false
	for _, x := range a.db.data.Users {
		if x.Email == strings.ToLower(strings.TrimSpace(in.Email)) {
			u = x
			found = true
			break
		}
	}
	if !found || bcrypt.CompareHashAndPassword([]byte(u.Password), []byte(in.Password)) != nil {
		fail(w, 401, "invalid email or password")
		return
	}
	token := id()
	a.db.data.Sessions[token] = session{UserID: u.ID, Expires: time.Now().Add(30 * 24 * time.Hour)}
	if err := a.db.save(); err != nil {
		fail(w, 500, "could not create session")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: a.secureCookie, MaxAge: 30 * 24 * 3600})
	reply(w, map[string]any{"token": token, "expires_at": a.db.data.Sessions[token].Expires, "user": map[string]string{"id": u.ID, "email": u.Email}})
}
func (a *app) logout(w http.ResponseWriter, r *http.Request, u user) {
	if r.Method != "POST" {
		fail(w, 405, "POST required")
		return
	}
	t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if c, e := r.Cookie("session"); t == "" && e == nil {
		t = c.Value
	}
	a.db.mu.Lock()
	delete(a.db.data.Sessions, t)
	_ = a.db.save()
	a.db.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	reply(w, map[string]bool{"ok": true})
}
func (a *app) me(w http.ResponseWriter, r *http.Request, u user) {
	reply(w, map[string]string{"id": u.ID, "email": u.Email})
}
func (a *app) devices(w http.ResponseWriter, r *http.Request, u user) {
	if r.Method == "GET" {
		a.db.mu.Lock()
		out := []device{}
		for _, d := range a.db.data.Devices {
			if d.UserID == u.ID {
				out = append(out, d)
			}
		}
		a.db.mu.Unlock()
		reply(w, out)
		return
	}
	if r.Method != "POST" {
		fail(w, 405, "GET or POST required")
		return
	}
	var in struct{ Name, Platform string }
	if !decode(w, r, &in) {
		return
	}
	if len(strings.TrimSpace(in.Name)) < 1 || len(in.Name) > 100 || len(in.Platform) > 50 {
		fail(w, 400, "invalid device")
		return
	}
	d := device{ID: id(), UserID: u.ID, Name: strings.TrimSpace(in.Name), Platform: strings.TrimSpace(in.Platform), Created: time.Now().UTC()}
	a.db.mu.Lock()
	a.db.data.Devices[d.ID] = d
	err := a.db.save()
	a.db.mu.Unlock()
	if err != nil {
		fail(w, 500, "could not save device")
		return
	}
	replyStatus(w, 201, d)
}
func (a *app) prepare(w http.ResponseWriter, r *http.Request, u user) {
	if r.Method != "POST" {
		fail(w, 405, "POST required")
		return
	}
	var in struct {
		Path, SHA256, DeviceID string
		Size                   int64
	}
	if !decode(w, r, &in) {
		return
	}
	p, e := cleanPath(in.Path)
	if e != nil || in.Size < 0 || in.Size > a.maxUpload || len(in.SHA256) != 64 {
		fail(w, 400, "invalid upload request")
		return
	}
	if _, e := hex.DecodeString(in.SHA256); e != nil {
		fail(w, 400, "invalid sha256")
		return
	}
	a.db.mu.Lock()
	d, ok := a.db.data.Devices[in.DeviceID]
	if !ok || d.UserID != u.ID {
		a.db.mu.Unlock()
		fail(w, 403, "unknown device")
		return
	}
	up := upload{ID: id(), UserID: u.ID, DeviceID: in.DeviceID, FilePath: p, SHA256: strings.ToLower(in.SHA256), Size: in.Size, Temp: filepath.Join(a.dataDir, "staging", id()+".part"), Created: time.Now().UTC()}
	a.db.data.Uploads[up.ID] = up
	e = a.db.save()
	a.db.mu.Unlock()
	if e != nil {
		fail(w, 500, "could not prepare upload")
		return
	}
	replyStatus(w, 201, map[string]any{"upload_id": up.ID, "upload_url": "/api/uploads/" + up.ID + "/content", "commit_url": "/api/uploads/" + up.ID + "/commit", "expires_at": up.Created.Add(time.Hour)})
}
func (a *app) uploadRoute(w http.ResponseWriter, r *http.Request, u user) {
	x := strings.TrimPrefix(r.URL.Path, "/api/uploads/")
	parts := strings.Split(x, "/")
	if len(parts) != 2 || parts[0] == "" {
		fail(w, 404, "not found")
		return
	}
	if parts[1] == "content" {
		a.putContent(w, r, u, parts[0])
		return
	}
	if parts[1] == "commit" {
		a.commit(w, r, u, parts[0])
		return
	}
	fail(w, 404, "not found")
}
func (a *app) putContent(w http.ResponseWriter, r *http.Request, u user, uploadID string) {
	if r.Method != "PUT" {
		fail(w, 405, "PUT required")
		return
	}
	a.db.mu.Lock()
	up, ok := a.db.data.Uploads[uploadID]
	a.db.mu.Unlock()
	if !ok || up.UserID != u.ID || time.Since(up.Created) > time.Hour {
		fail(w, 404, "upload not found or expired")
		return
	}
	if r.ContentLength > up.Size || (r.ContentLength >= 0 && r.ContentLength != up.Size) {
		fail(w, 400, "content length does not match")
		return
	}
	f, e := os.OpenFile(up.Temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		fail(w, 500, "could not stage upload")
		return
	}
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(r.Body, up.Size+1))
	closeErr := f.Close()
	if e != nil || closeErr != nil || n != up.Size || n > a.maxUpload {
		_ = os.Remove(up.Temp)
		fail(w, 400, "upload size mismatch")
		return
	}
	if hex.EncodeToString(h.Sum(nil)) != up.SHA256 {
		_ = os.Remove(up.Temp)
		fail(w, 422, "upload checksum mismatch")
		return
	}
	a.db.mu.Lock()
	up.Received = true
	a.db.data.Uploads[uploadID] = up
	e = a.db.save()
	a.db.mu.Unlock()
	if e != nil {
		fail(w, 500, "could not update upload")
		return
	}
	reply(w, map[string]any{"ok": true, "size": n, "sha256": up.SHA256})
}
func (a *app) commit(w http.ResponseWriter, r *http.Request, u user, uploadID string) {
	if r.Method != "POST" {
		fail(w, 405, "POST required")
		return
	}
	a.db.mu.Lock()
	up, ok := a.db.data.Uploads[uploadID]
	if !ok || up.UserID != u.ID || !up.Received {
		a.db.mu.Unlock()
		fail(w, 409, "upload is not ready")
		return
	}
	v := version{ID: id(), UserID: u.ID, FilePath: up.FilePath, SHA256: up.SHA256, DeviceID: up.DeviceID, Size: up.Size, Created: time.Now().UTC(), Metadata: map[string]string{"media_type": "pending", "thumbnail": "pending", "extraction": "pending"}}
	v.Blob = u.ID + "/" + v.ID
	blob, e := a.blobs.Commit(r.Context(), up.Temp, v.Blob, v.Size)
	if e != nil {
		a.db.mu.Unlock()
		fail(w, 500, "could not commit blob")
		return
	}
	v.Blob = blob
	key := fileKey(u.ID, up.FilePath)
	a.db.data.Versions[key] = append(a.db.data.Versions[key], v)
	delete(a.db.data.Uploads, uploadID)
	e = a.db.save()
	a.db.mu.Unlock()
	if e != nil {
		fail(w, 500, "metadata save failed; blob retained safely")
		return
	}
	replyStatus(w, 201, v)
}
func (a *app) files(w http.ResponseWriter, r *http.Request, u user) {
	if r.Method != "GET" {
		fail(w, 405, "GET required")
		return
	}
	prefix := strings.Trim(r.URL.Query().Get("path"), "/")
	if prefix != "" {
		var e error
		prefix, e = cleanPath(prefix)
		if e != nil {
			fail(w, 400, "invalid path")
			return
		}
		prefix += "/"
	}
	type item struct {
		Path      string   `json:"path"`
		Directory bool     `json:"directory"`
		Latest    *version `json:"latest,omitempty"`
	}
	dirs := map[string]bool{}
	out := []item{}
	a.db.mu.Lock()
	for key, vs := range a.db.data.Versions {
		if !strings.HasPrefix(key, fileKey(u.ID, prefix)) {
			continue
		}
		p := strings.TrimPrefix(key, fileKey(u.ID, ""))
		rest := strings.TrimPrefix(p, prefix)
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			d := prefix + rest[:i]
			dirs[d] = true
			continue
		}
		latest := vs[len(vs)-1]
		out = append(out, item{Path: p, Latest: &latest})
	}
	a.db.mu.Unlock()
	for d := range dirs {
		out = append(out, item{Path: d, Directory: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	reply(w, out)
}
func (a *app) history(w http.ResponseWriter, r *http.Request, u user) {
	p, e := cleanPath(r.URL.Query().Get("path"))
	if e != nil {
		fail(w, 400, "invalid path")
		return
	}
	a.db.mu.Lock()
	vs := append([]version(nil), a.db.data.Versions[fileKey(u.ID, p)]...)
	a.db.mu.Unlock()
	out := []version{}
	for i := len(vs) - 1; i >= 0; i-- {
		out = append(out, vs[i])
	}
	reply(w, out)
}
func (a *app) verify(w http.ResponseWriter, r *http.Request, u user) {
	p, e := cleanPath(r.URL.Query().Get("path"))
	if e != nil {
		fail(w, 400, "invalid path")
		return
	}
	a.db.mu.Lock()
	vs := a.db.data.Versions[fileKey(u.ID, p)]
	a.db.mu.Unlock()
	if len(vs) > 0 {
		v := vs[len(vs)-1]
		f, e := a.blobs.Open(r.Context(), v.Blob)
		if e != nil {
			fail(w, 404, "blob missing")
			return
		}
		h := sha256.New()
		_, e = io.Copy(h, f)
		f.Close()
		reply(w, map[string]any{"ok": e == nil && hex.EncodeToString(h.Sum(nil)) == v.SHA256, "version": v.ID, "sha256": v.SHA256})
		return
	}
	fail(w, 404, "file not found")
}
func (a *app) download(w http.ResponseWriter, r *http.Request, u user) {
	p, e := cleanPath(r.URL.Query().Get("path"))
	if e != nil {
		fail(w, 400, "invalid path")
		return
	}
	wanted := r.URL.Query().Get("version")
	a.db.mu.Lock()
	vs := a.db.data.Versions[fileKey(u.ID, p)]
	a.db.mu.Unlock()
	for i := len(vs) - 1; i >= 0; i-- {
		if wanted == "" || wanted == vs[i].ID {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(p)))
			f, err := a.blobs.Open(r.Context(), vs[i].Blob)
			if err != nil {
				fail(w, 404, "blob missing")
				return
			}
			defer f.Close()
			w.Header().Set("Content-Type", "application/octet-stream")
			if _, err := io.Copy(w, f); err != nil {
				log.Printf("download %s: %v", vs[i].ID, err)
			}
			return
		}
	}
	fail(w, 404, "file not found")
}
