// Package api implements the S3-compatible HTTP API.
package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/din14970/miniS3/internal/sigv4"
	"github.com/din14970/miniS3/internal/store"
)

// Server serves the S3 API for a single bucket plus the web UI.
type Server struct {
	Store  store.Store
	Bucket string
	Region string
	Auth   *sigv4.Auth
	UI     http.Handler // mounted under /ui; may be nil
}

func requestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rid := requestID()
	w.Header().Set("x-amz-request-id", rid)
	if strings.HasPrefix(r.URL.Path, "/ui") {
		if s.UI != nil {
			s.UI.ServeHTTP(w, r)
			return
		}
		writeError(w, r, rid, http.StatusNotFound, "NotFound", "UI disabled", r.URL.Path)
		return
	}
	if err := s.checkAuth(r); err != nil {
		writeAuthError(w, r, rid, err)
		return
	}
	s.route(w, r, rid)
}

func (s *Server) checkAuth(r *http.Request) error {
	if s.Auth == nil || !s.Auth.Enabled() {
		return nil
	}
	ph := r.Header.Get("X-Amz-Content-Sha256")
	if r.URL.Query().Get("X-Amz-Algorithm") != "" {
		ph = "UNSIGNED-PAYLOAD"
	}
	if strings.HasPrefix(ph, "STREAMING-") {
		return sigv4.ErrUnsupported
	}
	return s.Auth.Verify(r, ph)
}

func writeAuthError(w http.ResponseWriter, r *http.Request, rid string, err error) {
	switch {
	case errors.Is(err, sigv4.ErrNoAuth):
		writeError(w, r, rid, http.StatusForbidden, "AccessDenied", "Anonymous access is forbidden.", r.URL.Path)
	case errors.Is(err, sigv4.ErrBadCred):
		writeError(w, r, rid, http.StatusForbidden, "InvalidAccessKeyId", "The access key id is invalid.", r.URL.Path)
	case errors.Is(err, sigv4.ErrBadSig):
		writeError(w, r, rid, http.StatusForbidden, "SignatureDoesNotMatch", "The request signature does not match.", r.URL.Path)
	case errors.Is(err, sigv4.ErrExpired):
		writeError(w, r, rid, http.StatusForbidden, "AccessDenied", "Request has expired.", r.URL.Path)
	case errors.Is(err, sigv4.ErrUnsupported):
		writeError(w, r, rid, http.StatusBadRequest, "NotImplemented", "Streaming payload signatures are not supported.", r.URL.Path)
	default:
		writeError(w, r, rid, http.StatusBadRequest, "InvalidRequest", err.Error(), r.URL.Path)
	}
}

func (s *Server) route(w http.ResponseWriter, r *http.Request, rid string) {
	path := r.URL.Path
	if path == "/" || path == "" {
		if r.Method != http.MethodGet {
			writeError(w, r, rid, http.StatusMethodNotAllowed, "MethodNotAllowed", "method not allowed", "/")
			return
		}
		s.listBuckets(w, r, rid)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
	bucket := parts[0]
	if bucket != s.Bucket {
		writeError(w, r, rid, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist", "/"+bucket)
		return
	}
	if !s.Store.Exists() {
		writeError(w, r, rid, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist", "/"+bucket)
		return
	}
	if len(parts) == 1 {
		s.handleBucket(w, r, rid)
		return
	}
	key := parts[1]
	s.handleObject(w, r, rid, key)
}

func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	if err := xml.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode xml: %v", err)
	}
}

func writeError(w http.ResponseWriter, r *http.Request, rid string, status int, code, msg, resource string) {
	writeXML(w, status, ErrorResult{Code: code, Message: msg, Resource: resource, RequestID: rid})
}

// storeError maps store errors to S3 errors.
func storeError(w http.ResponseWriter, r *http.Request, rid string, err error, key string) {
	var nf *store.NotFoundError
	var uf *store.UploadNotFoundError
	switch {
	case errors.As(err, &nf):
		code := "NoSuchKey"
		if nf.VersionID != "" {
			code = "NoSuchVersion"
		}
		writeError(w, r, rid, http.StatusNotFound, code, "The specified key does not exist.", key)
	case errors.As(err, &uf):
		writeError(w, r, rid, http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist.", key)
	case errors.Is(err, store.ErrBucketNotEmpty):
		writeError(w, r, rid, http.StatusConflict, "BucketNotEmpty", "The bucket is not empty.", key)
	case errors.Is(err, store.ErrPartTooSmall):
		writeError(w, r, rid, http.StatusBadRequest, "EntityTooSmall", "Your proposed upload is smaller than the minimum allowed size.", key)
	case errors.Is(err, store.ErrInvalidPart):
		writeError(w, r, rid, http.StatusBadRequest, "InvalidPart", "One or more of the specified parts could not be found.", key)
	case errors.Is(err, store.ErrInvalidRange):
		w.Header().Set("Content-Range", "bytes */*")
		writeError(w, r, rid, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range is not satisfiable.", key)
	default:
		log.Printf("internal error: %v", err)
		writeError(w, r, rid, http.StatusInternalServerError, "InternalError", "We encountered an internal error.", key)
	}
}

func parseMaxKeys(v string, def int) int {
	if v == "" {
		return def
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
		if n > 100000 {
			return -1
		}
	}
	return n
}
