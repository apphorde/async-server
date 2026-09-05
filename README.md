# Storage Reader Server

A small self-hosted, single-node Dropbox-lite backend. The supplied Compose stack stores committed payloads in FileBin while MinIO is unavailable. Copy `.env.example` to `.env`, create a password-protected FileBin bin, fill in its ID and password, then run `docker compose up --build` and open `http://localhost:8080`. The first account may always be created; later registrations require `ALLOW_REGISTRATION=true`.

For production, terminate TLS in front of the service and set `SECURE_COOKIES=true`. Persist `/data`, keep it private, and back it up. Authentication accepts a browser session cookie or an Android-friendly `Authorization: Bearer <token>` header. Tokens are issued only by `POST /api/login`; do not embed credentials or tokens in an app binary.

Passwords must be 12 to 72 UTF-8 bytes because the server uses bcrypt. A normal password-manager generated password below that length is suitable.

## Container image

Build and push the image from this repository:

```sh
docker build -t ghcr.io/apphorde/async-server:latest .
docker push ghcr.io/apphorde/async-server:latest
```

The image contains no FileBin credentials. Provide `STORAGE_BACKEND=filebin`, `FILEBIN_URL`, `FILEBIN_ID`, and `FILEBIN_PASSWORD` only as deployment-time environment variables. Mount a persistent writable volume at `/data`; it contains local upload staging and the single-node metadata database. The process runs as UID `10001`. `/healthz` is an unauthenticated container health endpoint and returns `204` when the service is running.

Example runtime configuration:

```sh
docker run -d \
  --name async-server \
  --restart unless-stopped \
  -p 8080:8080 \
  -v async-server-data:/data \
  -e STORAGE_BACKEND=filebin \
  -e FILEBIN_URL=https://file.api.apphor.de \
  -e FILEBIN_ID \
  -e FILEBIN_PASSWORD \
  -e SECURE_COOKIES=true \
  ghcr.io/apphorde/async-server:latest
```

The `FILEBIN_ID` and `FILEBIN_PASSWORD` variables in this example are read from the deployment environment; they are not written into the image or command line. Put the container behind HTTPS before setting `SECURE_COOKIES=true`. Configure the reverse proxy to forward requests to port `8080` and use `/healthz` for its health check.

For Compose, copy `.env.example` to `.env`, set the FileBin values, and run:

```sh
docker compose up -d
```

## Android upload flow

1. `POST /api/login` with `email`, `password`; retain the returned token securely.
2. `POST /api/devices` with `name`, `platform`; retain returned `id` as the device token/identifier.
3. `POST /api/uploads/prepare` with `path`, `size`, lowercase SHA-256 `sha256`, and `device_id`.
4. Stream exact bytes with `PUT upload_url` and the bearer token.
5. `POST commit_url`. This atomically promotes the staged payload into an immutable version.
6. `GET /api/verify?path=...` validates the latest payload hash.

`GET /api/files?path=folder`, `GET /api/history?path=file`, and `GET /api/download?path=file&version=id` support browsing. Upload paths are relative POSIX paths and reject traversal and absolute paths. Derived metadata fields are deliberately placeholders (`media_type`, `thumbnail`, `extraction`) for a future worker.

## Blob storage

`STORAGE_BACKEND=filesystem` (default) stores payloads under `DATA_DIR/blobs`; it is intended for local development and Go tests. `STORAGE_BACKEND=s3` stores payloads through the S3 API and requires all of these settings:

- `S3_ENDPOINT`, for example `minio:9000` in Compose or `minio.example.net:443`.
- `S3_ACCESS_KEY` and `S3_SECRET_KEY`, supplied only to the server environment.
- `S3_BUCKET`, which the server creates if it does not exist.
- `S3_USE_SSL=true` for a TLS endpoint.

Uploads are locally staged under `DATA_DIR/staging`, checked against their declared size and SHA-256, then uploaded to an opaque immutable `userID/versionID` S3 key during commit. The server rejects an existing S3 key before commit; keys are 192-bit random identifiers. Download and verify stream from the selected backend. Do not expose MinIO credentials to Android clients or browsers. The JSON metadata store remains single-instance; use a transactional metadata database before scaling the server horizontally.

`STORAGE_BACKEND=filebin` stores committed versions in a FileBin bin and requires:

- `FILEBIN_URL`, currently `https://file.api.apphor.de`.
- `FILEBIN_ID`, the private FileBin bin identifier.
- `FILEBIN_PASSWORD`, the bin password.

Create a new bin with `POST /bin`, then enable a password with `PUT /lock/{binId}` before deploying this service. FileBin assigns a UUID to every object, so the Reader Vault metadata records that UUID rather than a filename. The server creates a new FileBin file for every committed version and never replaces it. FileBin is an interim single-bin blob store: it lacks resumable uploads, immutable-object enforcement, and server-side checksum verification, which Reader Vault compensates for with local staging and post-upload SHA-256 verification. Keep FileBin credentials only in the server environment.
