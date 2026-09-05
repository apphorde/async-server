package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanPath(t *testing.T) {
	for _, p := range []string{"a/b.txt", "folder/file"} {
		if _, err := cleanPath(p); err != nil {
			t.Fatalf("%q rejected: %v", p, err)
		}
	}
	for _, p := range []string{"../x", "/etc/passwd", "a/../../x", ""} {
		if _, err := cleanPath(p); err == nil {
			t.Fatalf("%q accepted", p)
		}
	}
}

func TestSHA(t *testing.T) {
	if sha([]byte("abc")) != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal("wrong digest")
	}
}

func TestUploadCommitAndDownload(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "staging"), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := newStore(filepath.Join(dir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := newFilesystemBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db, dataDir: dir, blobs: blobs, maxUpload: 1024 * 1024}
	post := func(handler http.HandlerFunc, body string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		handler(r, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body)))
		return r
	}
	if r := post(a.register, `{"Email":"a@example.test","Password":"long secure password"}`); r.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", r.Code, r.Body.String())
	}
	login := post(a.login, `{"Email":"a@example.test","Password":"long secure password"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("login: %d", login.Code)
	}
	var credentials struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(login.Body).Decode(&credentials); err != nil {
		t.Fatal(err)
	}
	authed := func(h func(http.ResponseWriter, *http.Request, user), method, target string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+credentials.Token)
		r := httptest.NewRecorder()
		a.auth(h).ServeHTTP(r, req)
		return r
	}
	deviceResponse := authed(a.devices, http.MethodPost, "/api/devices", []byte(`{"Name":"test phone","Platform":"android"}`))
	if deviceResponse.Code != http.StatusCreated {
		t.Fatalf("device: %d %s", deviceResponse.Code, deviceResponse.Body.String())
	}
	var d device
	if err := json.NewDecoder(deviceResponse.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	payload := []byte("immutable data")
	s := sha256.Sum256(payload)
	prepareBody := []byte(`{"Path":"books/a.txt","SHA256":"` + hex.EncodeToString(s[:]) + `","DeviceID":"` + d.ID + `","Size":14}`)
	prepared := authed(a.prepare, http.MethodPost, "/api/uploads/prepare", prepareBody)
	if prepared.Code != http.StatusCreated {
		t.Fatalf("prepare: %d %s", prepared.Code, prepared.Body.String())
	}
	var urls struct {
		UploadID string `json:"upload_id"`
	}
	if err := json.NewDecoder(prepared.Body).Decode(&urls); err != nil {
		t.Fatal(err)
	}
	put := authed(a.uploadRoute, http.MethodPut, "/api/uploads/"+urls.UploadID+"/content", payload)
	if put.Code != http.StatusOK {
		t.Fatalf("put: %d %s", put.Code, put.Body.String())
	}
	committed := authed(a.uploadRoute, http.MethodPost, "/api/uploads/"+urls.UploadID+"/commit", nil)
	if committed.Code != http.StatusCreated {
		t.Fatalf("commit: %d %s", committed.Code, committed.Body.String())
	}
	download := authed(a.download, http.MethodGet, "/api/download?path=books/a.txt", nil)
	if download.Code != http.StatusOK || !bytes.Equal(download.Body.Bytes(), payload) {
		t.Fatalf("download: %d %q", download.Code, download.Body.Bytes())
	}
}

func TestFileBinBlobStore(t *testing.T) {
	var payload []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /f/bin":
			if user, password, ok := r.BasicAuth(); !ok || user != "reader-vault" || password != "secret" {
				t.Fatal("missing FileBin basic auth")
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"fileId":"file-uuid"}`))
		case "PUT /f/bin/file-uuid":
			payload, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusAccepted)
		case "GET /f/bin/file-uuid":
			_, _ = w.Write(payload)
		default:
			t.Fatalf("unexpected FileBin request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	store, err := newFileBinBlobStore(server.URL, "bin", "secret")
	if err != nil {
		t.Fatal(err)
	}
	store.client = server.Client()
	staged := filepath.Join(t.TempDir(), "staged")
	if err = os.WriteFile(staged, []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := store.Commit(context.Background(), staged, "user/version", 7)
	if err != nil || key != "file-uuid" {
		t.Fatalf("commit: key=%q err=%v", key, err)
	}
	stream, err := store.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	got, err := io.ReadAll(stream)
	if err != nil || !bytes.Equal(got, []byte("content")) {
		t.Fatalf("open: %q %v", got, err)
	}
}

func TestFilesystemBlobStoreDoesNotReplaceCommittedBlob(t *testing.T) {
	dir := t.TempDir()
	s, err := newFilesystemBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	if err := os.WriteFile(first, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(context.Background(), first, "user/version", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(context.Background(), second, "user/version", 6); err == nil {
		t.Fatal("replaced immutable blob")
	}
	r, err := s.Open(context.Background(), "user/version")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b := make([]byte, 5)
	if _, err := r.Read(b); err != nil || string(b) != "first" {
		t.Fatalf("committed blob changed: %q, %v", b, err)
	}
}
