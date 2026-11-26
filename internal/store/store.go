package store

import (
	"errors"
	"io"
	"time"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrNoSuchKey     = errors.New("no such key")
	ErrNoSuchUpload  = errors.New("no such upload")
	ErrBucketNotEmpty = errors.New("bucket not empty")
	ErrUploadExists  = errors.New("upload already exists")
	ErrInvalidPart   = errors.New("invalid part")
	ErrPartTooSmall  = errors.New("part too small")
	ErrInvalidRange  = errors.New("invalid range")
)

// NotFoundError describes a missing object version or key.
type NotFoundError struct {
	Key       string
	VersionID string
}

func (e *NotFoundError) Error() string {
	if e.VersionID != "" {
		return "no such key: " + e.Key + " (version " + e.VersionID + ")"
	}
	return "no such key: " + e.Key
}

func (e *NotFoundError) Unwrap() error { return ErrNotFound }

// UploadNotFoundError describes a missing multipart upload.
type UploadNotFoundError struct {
	UploadID string
}

func (e *UploadNotFoundError) Error() string { return "no such upload: " + e.UploadID }

func (e *UploadNotFoundError) Unwrap() error { return ErrNoSuchUpload }

// Versioning states.
const (
	VersioningUnversioned = ""
	VersioningEnabled     = "Enabled"
	VersioningSuspended   = "Suspended"
)

// NullVersionID is the version id used for objects in unversioned/suspended buckets.
const NullVersionID = "null"

// Version describes one object version.
type Version struct {
	Key          string
	VersionID    string
	Size         int64
	ETag         string
	ContentType  string
	LastModified time.Time
	DeleteMarker bool
	IsLatest     bool
}

// Object is a version plus its content stream.
type Object struct {
	Version
	Body io.ReadSeeker
}

// Close releases the object body.
func (o *Object) Close() error {
	if c, ok := o.Body.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// ListParams controls listing of objects or versions.
type ListParams struct {
	Prefix            string
	Delimiter         string
	MaxKeys           int
	StartAfter        string // ListObjectsV2
	ContinuationToken string // ListObjectsV2
	Marker            string // ListObjects v1
	KeyMarker         string // ListObjectVersions
	VersionIDMarker   string // ListObjectVersions
	FetchOwner        bool
}

// ListResult is a page of object versions and/or common prefixes.
type ListResult struct {
	Objects               []Version // sorted by key asc; for version listings, versions of a key newest first
	CommonPrefixes        []string
	IsTruncated           bool
	NextMarker            string // v1 marker (last key)
	NextContinuationToken string
	NextKeyMarker         string
	NextVersionIDMarker   string
}

// Part is a part to complete a multipart upload with.
type Part struct {
	PartNumber int
	ETag       string
}

// UploadInfo describes an in-progress multipart upload.
type UploadInfo struct {
	Key         string
	UploadID    string
	Initiated   time.Time
}

// PartInfo describes an uploaded part.
type PartInfo struct {
	PartNumber   int
	Size         int64
	ETag         string
	LastModified time.Time
}

// ListUploadsParams controls ListMultipartUploads.
type ListUploadsParams struct {
	Prefix         string
	Delimiter      string
	MaxUploads     int
	KeyMarker      string
	UploadIDMarker string
}

// ListUploadsResult is a page of in-progress uploads.
type ListUploadsResult struct {
	Uploads             []UploadInfo
	CommonPrefixes      []string
	IsTruncated         bool
	NextKeyMarker       string
	NextUploadIDMarker  string
}

// ListPartsResult is a page of parts of an upload.
type ListPartsResult struct {
	Parts         []PartInfo
	IsTruncated   bool
	NextMarker    int
}

// Store is the storage backend for a single bucket.
type Store interface {
	Exists() bool
	CreateBucket() error
	DeleteBucket() error
	Created() time.Time
	// ListObjects lists object versions (latest only unless versions requested).
	ListObjects(p ListParams, versions bool) (*ListResult, error)
	GetObject(key, versionID string) (*Object, error)
	HeadObject(key, versionID string) (*Version, error)
	PutObject(key string, body io.Reader, size int64, contentType string) (*Version, error)
	CopyObject(dstKey, srcKey, srcVersionID, contentType string) (*Version, error)
	// DeleteObject deletes a specific version, or adds a delete marker when
	// versionID is empty and versioning is enabled. Returns the resulting
	// version (delete marker) when one was created.
	DeleteObject(key, versionID string) (*Version, error)
	CreateMultipartUpload(key, contentType string) (string, error)
	UploadPart(uploadID string, partNumber int, body io.Reader, size int64) (string, error)
	CompleteMultipartUpload(uploadID string, parts []Part) (*Version, error)
	AbortMultipartUpload(uploadID string) error
	ListMultipartUploads(p ListUploadsParams) (*ListUploadsResult, error)
	ListParts(uploadID string, maxParts int, partNumberMarker int) (*ListPartsResult, error)
	GetVersioning() string
	PutVersioning(status string) error
	// Close releases resources.
	Close() error
}
