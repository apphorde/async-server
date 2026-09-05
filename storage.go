package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// blobStore stores committed immutable payloads. Upload staging stays local so
// both backends receive an already size- and checksum-validated file.
type blobStore interface {
	Commit(context.Context, string, string, int64) (string, error)
	Open(context.Context, string) (io.ReadCloser, error)
}

type filesystemBlobStore struct{ root string }

func newFilesystemBlobStore(root string) (*filesystemBlobStore, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	return &filesystemBlobStore{root: root}, nil
}
func (s *filesystemBlobStore) blobPath(key string) (string, error) {
	if strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("invalid blob key")
	}
	return filepath.Join(s.root, filepath.FromSlash(key)), nil
}
func (s *filesystemBlobStore) Commit(_ context.Context, staged, key string, _ int64) (string, error) {
	destination, err := s.blobPath(key)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return "", err
	}
	// Link refuses replacement, preserving immutable committed versions.
	if err := os.Link(staged, destination); err != nil {
		return "", fmt.Errorf("commit blob: %w", err)
	}
	if err := os.Remove(staged); err != nil {
		return "", err
	}
	return key, nil
}
func (s *filesystemBlobStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := s.blobPath(key)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

type minioBlobStore struct {
	client *minio.Client
	bucket string
}

func newMinioBlobStore(ctx context.Context, endpoint, accessKey, secretKey, bucket string, secure bool) (*minioBlobStore, error) {
	if endpoint == "" || accessKey == "" || secretKey == "" || bucket == "" {
		return nil, fmt.Errorf("S3_ENDPOINT, S3_ACCESS_KEY, S3_SECRET_KEY, and S3_BUCKET are required for s3 storage")
	}
	c, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(accessKey, secretKey, ""), Secure: secure})
	if err != nil {
		return nil, fmt.Errorf("create S3 client: %w", err)
	}
	exists, err := c.BucketExists(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("check S3 bucket: %w", err)
	}
	if !exists {
		if err := c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("create S3 bucket: %w", err)
		}
	}
	return &minioBlobStore{client: c, bucket: bucket}, nil
}
func (s *minioBlobStore) Commit(ctx context.Context, staged, key string, size int64) (string, error) {
	if _, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err == nil {
		return "", fmt.Errorf("refusing to replace immutable S3 blob")
	} else if code := minio.ToErrorResponse(err).Code; code != "NoSuchKey" && code != "NoSuchObject" {
		return "", fmt.Errorf("check immutable S3 blob: %w", err)
	}
	f, err := os.Open(staged)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := s.client.PutObject(ctx, s.bucket, key, f, size, minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return "", fmt.Errorf("store S3 blob: %w", err)
	}
	if info.Size != size {
		return "", fmt.Errorf("S3 stored unexpected size: got %d, want %d", info.Size, size)
	}
	if err = os.Remove(staged); err != nil {
		return "", fmt.Errorf("remove staged blob: %w", err)
	}
	return key, nil
}

// fileBinBlobStore uses FileBin only for opaque bytes. Reader Vault retains
// paths, versions, and integrity metadata so FileBin's mutable API cannot
// change the backup model.
type fileBinBlobStore struct {
	baseURL, binID, password string
	client                   *http.Client
}

func newFileBinBlobStore(baseURL, binID, password string) (*fileBinBlobStore, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.HasPrefix(baseURL, "https://") || binID == "" || password == "" {
		return nil, fmt.Errorf("FILEBIN_URL (HTTPS), FILEBIN_ID, and FILEBIN_PASSWORD are required for filebin storage")
	}
	return &fileBinBlobStore{baseURL: baseURL, binID: binID, password: password, client: &http.Client{Timeout: 10 * time.Minute}}, nil
}
func (s *fileBinBlobStore) request(ctx context.Context, method, endpoint string, body io.Reader, contentType string) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, method, s.baseURL+endpoint, body)
	if err != nil {
		return nil, err
	}
	r.SetBasicAuth("reader-vault", s.password)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	return s.client.Do(r)
}
func (s *fileBinBlobStore) Commit(ctx context.Context, staged, key string, size int64) (string, error) {
	metadata, err := json.Marshal(map[string]string{"name": "reader-vault/" + key, "type": "application/octet-stream"})
	if err != nil {
		return "", err
	}
	created, err := s.request(ctx, http.MethodPost, "/f/"+s.binID, strings.NewReader(string(metadata)), "application/json")
	if err != nil {
		return "", fmt.Errorf("create FileBin file: %w", err)
	}
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create FileBin file: HTTP %d", created.StatusCode)
	}
	var result struct {
		FileID string `json:"fileId"`
	}
	if err = json.NewDecoder(created.Body).Decode(&result); err != nil || result.FileID == "" {
		return "", fmt.Errorf("read FileBin file ID: %w", err)
	}
	f, err := os.Open(staged)
	if err != nil {
		return "", err
	}
	defer f.Close()
	stored, err := s.request(ctx, http.MethodPut, "/f/"+s.binID+"/"+result.FileID, f, "application/octet-stream")
	if err != nil {
		return "", fmt.Errorf("upload FileBin file: %w", err)
	}
	defer stored.Body.Close()
	if stored.StatusCode/100 != 2 {
		return "", fmt.Errorf("upload FileBin file: HTTP %d", stored.StatusCode)
	}
	if err = os.Remove(staged); err != nil {
		return "", fmt.Errorf("remove staged blob: %w", err)
	}
	return result.FileID, nil
}
func (s *fileBinBlobStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	response, err := s.request(ctx, http.MethodGet, "/f/"+s.binID+"/"+key, nil, "")
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("read FileBin file: HTTP %d", response.StatusCode)
	}
	return response.Body, nil
}
func (s *minioBlobStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	o, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	// GetObject is lazy; Stat forces authentication/not-found errors before HTTP headers are written.
	if _, err = o.Stat(); err != nil {
		o.Close()
		return nil, err
	}
	return o, nil
}
