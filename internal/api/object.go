package api

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/din14970/miniS3/internal/store"
)

func (s *Server) handleObject(w http.ResponseWriter, r *http.Request, rid, key string) {
	q := r.URL.Query()
	switch r.Method {
	case http.MethodGet:
		switch {
		case q.Has("uploadId"):
			s.listParts(w, r, rid, key, q.Get("uploadId"))
		case q.Has("tagging"):
			writeXML(w, http.StatusOK, Tagging{Xmlns: xmlns})
		case q.Has("acl"):
			s.writeACL(w)
		default:
			s.getObject(w, r, rid, key, q.Get("versionId"))
		}
	case http.MethodHead:
		s.headObject(w, r, rid, key, q.Get("versionId"))
	case http.MethodPut:
		switch {
		case q.Has("uploadId") && q.Has("partNumber"):
			if r.Header.Get("x-amz-copy-source") != "" {
				s.uploadPartCopy(w, r, rid, key, q.Get("uploadId"), q.Get("partNumber"))
			} else {
				s.uploadPart(w, r, rid, key, q.Get("uploadId"), q.Get("partNumber"))
			}
		case r.Header.Get("x-amz-copy-source") != "":
			s.copyObject(w, r, rid, key)
		case q.Has("tagging"):
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		default:
			s.putObject(w, r, rid, key)
		}
	case http.MethodPost:
		switch {
		case q.Has("uploads"):
			s.createMultipartUpload(w, r, rid, key)
		case q.Has("uploadId"):
			s.completeMultipartUpload(w, r, rid, key, q.Get("uploadId"))
		default:
			writeError(w, r, rid, http.StatusMethodNotAllowed, "MethodNotAllowed", "method not allowed", r.URL.Path)
		}
	case http.MethodDelete:
		if q.Has("uploadId") {
			if err := s.Store.AbortMultipartUpload(q.Get("uploadId")); err != nil {
				storeError(w, r, rid, err, key)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.deleteObject(w, r, rid, key, q.Get("versionId"))
	default:
		writeError(w, r, rid, http.StatusMethodNotAllowed, "MethodNotAllowed", "method not allowed", r.URL.Path)
	}
}

// streamBody copies the request body to a temp file, computing md5, sha256 and
// crc32c along the way. It returns the temp file (rewound), its size and the
// digests.
func streamBody(w http.ResponseWriter, r *http.Request) (*os.File, int64, []byte, []byte, uint32, error) {
	f, err := os.CreateTemp("", "minis3-upload-*")
	if err != nil {
		return nil, 0, nil, nil, 0, err
	}
	md5h := md5.New()
	sha := sha256.New()
	crc := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	n, err := io.Copy(io.MultiWriter(f, md5h, sha, crc), r.Body)
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, 0, nil, nil, 0, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, 0, nil, nil, 0, err
	}
	return f, n, md5h.Sum(nil), sha.Sum(nil), crc.Sum32(), nil
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, rid, key string) {
	if err := store.ValidateKey(key); err != nil {
		writeError(w, r, rid, http.StatusBadRequest, "InvalidObjectName", err.Error(), key)
		return
	}
	f, size, md5sum, shaSum, crc, err := streamBody(w, r)
	if err != nil {
		writeError(w, r, rid, http.StatusBadRequest, "IncompleteBody", err.Error(), key)
		return
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	if err := checkPayloadIntegrity(r, md5sum, shaSum); err != nil {
		writeError(w, r, rid, http.StatusBadRequest, errCode(err), err.Error(), key)
		return
	}
	v, err := s.Store.PutObject(key, f, size, r.Header.Get("Content-Type"))
	if err != nil {
		storeError(w, r, rid, err, key)
		return
	}
	w.Header().Set("ETag", `"`+v.ETag+`"`)
	if v.VersionID != "" {
		w.Header().Set("x-amz-version-id", v.VersionID)
	}
	writeChecksumHeaders(w, r, crc)
	w.WriteHeader(http.StatusOK)
}

type integrityError struct{ code, msg string }

func (e *integrityError) Error() string { return e.msg }

func errCode(err error) string {
	var ie *integrityError
	if errors.As(err, &ie) {
		return ie.code
	}
	return "BadDigest"
}

func checkPayloadIntegrity(r *http.Request, md5sum, shaSum []byte) error {
	if h := r.Header.Get("X-Amz-Content-Sha256"); h != "" && h != "UNSIGNED-PAYLOAD" && !strings.HasPrefix(h, "STREAMING-") {
		want, err := hex.DecodeString(h)
		if err == nil && len(want) == sha256.Size && !equalBytes(want, shaSum) {
			return &integrityError{"XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed."}
		}
	}
	if h := r.Header.Get("Content-MD5"); h != "" {
		want, err := base64.StdEncoding.DecodeString(h)
		if err == nil && !equalBytes(want, md5sum) {
			return &integrityError{"BadDigest", "The Content-MD5 you specified did not match what we received."}
		}
	}
	return nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func writeChecksumHeaders(w http.ResponseWriter, r *http.Request, crc uint32) {
	alg := r.Header.Get("x-amz-checksum-algorithm")
	if alg == "" {
		alg = r.Header.Get("x-amz-sdk-checksum-algorithm")
	}
	if strings.EqualFold(alg, "CRC32") {
		b := []byte{byte(crc >> 24), byte(crc >> 16), byte(crc >> 8), byte(crc)}
		w.Header().Set("x-amz-checksum-crc32", base64.StdEncoding.EncodeToString(b))
	}
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, rid, key, versionID string) {
	obj, err := s.Store.GetObject(key, versionID)
	if err != nil {
		storeError(w, r, rid, err, key)
		return
	}
	defer obj.Close()
	if obj.DeleteMarker {
		w.Header().Set("x-amz-delete-marker", "true")
		if versionID != "" {
			w.Header().Set("x-amz-version-id", versionID)
		}
		writeError(w, r, rid, http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource.", key)
		return
	}
	s.serveObject(w, r, rid, obj, key, versionID)
}

func (s *Server) headObject(w http.ResponseWriter, r *http.Request, rid, key, versionID string) {
	v, err := s.Store.HeadObject(key, versionID)
	if err != nil {
		storeError(w, r, rid, err, key)
		return
	}
	if v.DeleteMarker {
		w.Header().Set("x-amz-delete-marker", "true")
		w.Header().Set("x-amz-version-id", v.VersionID)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeObjectHeaders(w, v)
	w.WriteHeader(http.StatusOK)
}

func writeObjectHeaders(w http.ResponseWriter, v *store.Version) {
	w.Header().Set("Content-Type", v.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(v.Size, 10))
	w.Header().Set("ETag", `"`+v.ETag+`"`)
	w.Header().Set("Last-Modified", httpTime(v.LastModified))
	w.Header().Set("Accept-Ranges", "bytes")
	if v.VersionID != "" {
		w.Header().Set("x-amz-version-id", v.VersionID)
	}
}

// serveObject writes the object body, honoring a single Range header.
func (s *Server) serveObject(w http.ResponseWriter, r *http.Request, rid string, obj *store.Object, key, versionID string) {
	size := obj.Size
	rangeHeader := r.Header.Get("Range")
	start, length := int64(0), size
	partial := false
	if rangeHeader != "" {
		st, ln, ok := parseRange(rangeHeader, size)
		if !ok {
			w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
			writeError(w, r, rid, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range is not satisfiable.", key)
			return
		}
		start, length = st, ln
		partial = true
	}
	writeObjectHeaders(w, &obj.Version)
	if partial {
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(start+length-1, 10)+"/"+strconv.FormatInt(size, 10))
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if r.Method == http.MethodGet {
		if _, err := obj.Body.Seek(start, io.SeekStart); err != nil {
			return
		}
		io.CopyN(w, obj.Body, length)
	}
}

// parseRange parses a single-range "bytes=..." header.
func parseRange(h string, size int64) (int64, int64, bool) {
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(h, "bytes=")
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = spec[:i]
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	if parts[0] == "" {
		// suffix range: last N bytes
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, n, true
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if parts[1] != "" {
		e, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || e < start {
			return 0, 0, false
		}
		if e < end {
			end = e
		}
	}
	return start, end - start + 1, true
}

func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request, rid, key, versionID string) {
	if err := store.ValidateKey(key); err != nil {
		writeError(w, r, rid, http.StatusBadRequest, "InvalidObjectName", err.Error(), key)
		return
	}
	v, err := s.Store.DeleteObject(key, versionID)
	if err != nil {
		storeError(w, r, rid, err, key)
		return
	}
	if v != nil {
		w.Header().Set("x-amz-version-id", v.VersionID)
		if v.DeleteMarker {
			w.Header().Set("x-amz-delete-marker", "true")
		}
	} else if versionID != "" {
		w.Header().Set("x-amz-version-id", versionID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseCopySource parses an x-amz-copy-source header into bucket, key, versionID.
func parseCopySource(h string) (string, string, string, error) {
	h = strings.TrimSpace(h)
	h = strings.TrimPrefix(h, "/")
	var query string
	if i := strings.IndexByte(h, '?'); i >= 0 {
		query = h[i+1:]
		h = h[:i]
	}
	h = strings.TrimPrefix(h, "/")
	parts := strings.SplitN(h, "/", 2)
	if len(parts) != 2 {
		return "", "", "", errBadCopySource
	}
	bucket := parts[0]
	key, err := url.PathUnescape(parts[1])
	if err != nil {
		return "", "", "", err
	}
	versionID := ""
	if query != "" {
		vals, _ := url.ParseQuery(query)
		versionID = vals.Get("versionId")
	}
	return bucket, key, versionID, nil
}

var errBadCopySource = &copySourceError{}

type copySourceError struct{}

func (e *copySourceError) Error() string { return "invalid copy source" }

func (s *Server) copyObject(w http.ResponseWriter, r *http.Request, rid, dstKey string) {
	src := r.Header.Get("x-amz-copy-source")
	srcBucket, srcKey, srcVersion, err := parseCopySource(src)
	if err != nil || srcBucket != s.Bucket {
		writeError(w, r, rid, http.StatusNotFound, "NoSuchKey", "The specified copy source does not exist.", dstKey)
		return
	}
	contentType := ""
	if strings.EqualFold(r.Header.Get("x-amz-metadata-directive"), "REPLACE") {
		contentType = r.Header.Get("Content-Type")
	}
	v, err := s.Store.CopyObject(dstKey, srcKey, srcVersion, contentType)
	if err != nil {
		storeError(w, r, rid, err, dstKey)
		return
	}
	if v.VersionID != "" {
		w.Header().Set("x-amz-version-id", v.VersionID)
	}
	writeXML(w, http.StatusOK, CopyObjectResult{
		Xmlns:        xmlns,
		ETag:         `"` + v.ETag + `"`,
		LastModified: s3Time(v.LastModified),
	})
}
