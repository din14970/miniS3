# miniS3

A minimal Go application that serves the **S3 API on top of a local folder** — like a tiny single-node, single-bucket MinIO. Point it at a directory, and boto3 (or any S3 client) can store and retrieve objects from it over HTTP. A small built-in web UI lets you browse, upload, download and delete files from a browser.

## Features

- **S3-compatible API** (path-style: `http://host:port/<bucket>/<key>`):
  - Buckets: `ListBuckets`, `CreateBucket`, `HeadBucket`, `DeleteBucket`, `GetBucketLocation`, `GetBucketVersioning`/`PutBucketVersioning`, ACL stubs
  - Objects: `PutObject`, `GetObject` (incl. `Range` requests), `HeadObject`, `DeleteObject`, `CopyObject`, `DeleteObjects` (batch)
  - Listing: `ListObjects` (V1) and `ListObjectsV2` with `prefix`, `delimiter`, `max-keys`, `marker`/`continuation-token`/`start-after`, `encoding-type=url`; `ListObjectVersions`
  - **Multipart uploads**: `CreateMultipartUpload`, `UploadPart`, `UploadPartCopy`, `CompleteMultipartUpload`, `AbortMultipartUpload`, `ListParts`, `ListMultipartUploads`
  - **Object versioning**: enable/suspend, per-version GET/HEAD/DELETE/COPY, delete markers, `versionId` query parameter
  - S3-style XML responses and error codes (`NoSuchKey`, `NoSuchBucket`, `NoSuchUpload`, `SignatureDoesNotMatch`, ...), ETags (MD5; multipart `md5-of-md5s-N`), CRC32 checksum echo
  - Auth: permissive by default (any credentials, like a dev server); set `--access-key`/`--secret-key` to require valid **AWS SigV4** (header + presigned query) or legacy **SigV2** signatures — the ones boto3 generates
- **Pluggable metadata storage** (`--db`):
  - `auto` / `file` — SQLite file (`<data-dir>/.minis3.db`, pure-Go `modernc.org/sqlite`, no CGO)
  - `memory` — in-memory SQLite
  - `none` — no database at all; metadata lives in `meta.json` sidecar files, listing is done by walking the folder
  - A fresh/empty SQLite DB **populates itself automatically** on startup by scanning the data directory (object versions, delete markers, pending multipart uploads, versioning status)
- **Web UI** at `/ui` — plain HTML + CSS + [htmx](https://htmx.org) (vendored, no CDN) + a little vanilla JS: folder-style browsing with prefix navigation, upload with progress bar, download, delete, per-object version history, versioning toggle
- Single bucket, single node; no RBAC (by design)

## Quick start

```bash
go build -o minis3 .
./minis3 --addr :9000 --data-dir ./data --bucket mybucket
```

Then use it with boto3:

```python
import boto3
from botocore.config import Config

s3 = boto3.client(
    "s3",
    endpoint_url="http://localhost:9000",
    aws_access_key_id="anything",      # permissive mode accepts anything
    aws_secret_access_key="anything",
    region_name="us-east-1",
    config=Config(s3={"addressing_style": "path"}),
)
s3.put_object(Bucket="mybucket", Key="hello.txt", Body=b"hello world")
print(s3.get_object(Bucket="mybucket", Key="hello.txt")["Body"].read())
```

Open the web UI at <http://localhost:9000/ui/>.

## Flags

| Flag | Default | Description |
|---|---|---|
| `--addr` | `:9000` | listen address for S3 API + UI |
| `--data-dir` | `./data` | local folder backing the bucket |
| `--bucket` | `bucket` | name of the single served bucket |
| `--db` | `auto` | `auto` / `file` / `memory` / `none` — metadata storage |
| `--db-path` | `<data-dir>/.minis3.db` | SQLite file location when `--db=file` |
| `--access-key` / `--secret-key` | *(env `MINIS3_ACCESS_KEY` / `MINIS3_SECRET_KEY`)* | if set, requests must carry a valid SigV4/SigV2 signature |
| `--region` | `us-east-1` | region reported by `GetBucketLocation` |
| `--no-ui` | `false` | disable the web UI |

Examples:

```bash
# ad-hoc inside a folder, no database at all
./minis3 --data-dir /tmp/scratch --db none --addr :9000

# sqlite in memory (fast, nothing persisted except object bytes)
./minis3 --data-dir ./data --db memory

# require signed requests (boto3 must use these credentials)
./minis3 --access-key AKIAEXAMPLE --secret-key secretsecretsecret
```

## On-disk layout

```
<data-dir>/
  .minis3.db               # sqlite metadata (unless --db none|memory)
  <bucket>/
    .bucket.json           # bucket config (versioning status, creation time)
    objects/<key...>/.v/<version-id>/
      data                 # object bytes
      meta.json            # size, etag, content type, timestamps, delete-marker flag
    uploads/<upload-id>/   # in-progress multipart uploads
      meta.json
      parts/<n>.part, <n>.meta.json
```

Keys are stored as nested directories with percent-encoded segments, so arbitrary S3 keys (spaces, unicode, `?`, `&`, trailing `/`, ...) are safe on any filesystem. The `meta.json` sidecars are what allow the SQLite DB to rebuild itself by scanning when it starts empty.

## Testing

```bash
go test ./...                 # unit tests (store backends, API handlers)
tests/run_integration.sh      # builds the binary and runs a boto3 suite against
                              # all DB modes + auto-populate + SigV4/SigV2 auth
```

The integration suite (`tests/s3_integration.py`) exercises ~75 boto3 checks: put/get/head/delete, ranges, listing V1/V2 with pagination, copy, batch delete, full multipart flows, versioning (markers, per-version access, suspend), presigned URLs, error codes, and the web UI endpoints.

## Limitations

- One bucket, one node, path-style addressing only (virtual-host style is not supported)
- No RBAC/policies, no encryption, no storage classes beyond `STANDARD`, no cross-region replication
- Streaming (`aws-chunked`) payload signatures are rejected when auth is enabled; boto3's default payload signing works
- Listing a very large bucket in `--db none` mode walks the whole tree (use SQLite for performance)

## Layout

```
main.go                  flags + wiring
internal/store/          storage backends: fs (no DB), sqlite (memory/file), shared disk layout + listing
internal/api/            S3 HTTP API: routing, XML types, bucket/object/multipart handlers
internal/sigv4/          SigV4 + SigV2 verification
internal/web/            web UI (embedded templates/static, htmx vendored)
tests/                   boto3 integration suite + runner
```
