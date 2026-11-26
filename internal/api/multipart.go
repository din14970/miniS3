package api

import (
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/din14970/miniS3/internal/store"
)

func (s *Server) createMultipartUpload(w http.ResponseWriter, r *http.Request, rid, key string) {
	if err := store.ValidateKey(key); err != nil {
		writeError(w, r, rid, http.StatusBadRequest, "InvalidObjectName", err.Error(), key)
		return
	}
	uid, err := s.Store.CreateMultipartUpload(key, r.Header.Get("Content-Type"))
	if err != nil {
		storeError(w, r, rid, err, key)
		return
	}
	writeXML(w, http.StatusOK, CreateMultipartUploadResult{
		Xmlns:    xmlns,
		Bucket:   s.Bucket,
		Key:      key,
		UploadID: uid,
	})
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, rid, key, uploadID, partNumberStr string) {
	partNumber, err := strconv.Atoi(partNumberStr)
	if err != nil || partNumber < 1 || partNumber > 10000 {
		writeError(w, r, rid, http.StatusBadRequest, "InvalidArgument", "invalid part number", key)
		return
	}
	f, size, _, _, _, err := streamBody(w, r)
	if err != nil {
		writeError(w, r, rid, http.StatusBadRequest, "IncompleteBody", err.Error(), key)
		return
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	etag, err := s.Store.UploadPart(uploadID, partNumber, f, size)
	if err != nil {
		storeError(w, r, rid, err, key)
		return
	}
	w.Header().Set("ETag", `"`+etag+`"`)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) uploadPartCopy(w http.ResponseWriter, r *http.Request, rid, key, uploadID, partNumberStr string) {
	partNumber, err := strconv.Atoi(partNumberStr)
	if err != nil || partNumber < 1 || partNumber > 10000 {
		writeError(w, r, rid, http.StatusBadRequest, "InvalidArgument", "invalid part number", key)
		return
	}
	srcBucket, srcKey, srcVersion, err := parseCopySource(r.Header.Get("x-amz-copy-source"))
	if err != nil || srcBucket != s.Bucket {
		writeError(w, r, rid, http.StatusNotFound, "NoSuchKey", "The specified copy source does not exist.", key)
		return
	}
	obj, err := s.Store.GetObject(srcKey, srcVersion)
	if err != nil {
		storeError(w, r, rid, err, srcKey)
		return
	}
	defer obj.Close()
	etag, err := s.Store.UploadPart(uploadID, partNumber, obj.Body, obj.Size)
	if err != nil {
		storeError(w, r, rid, err, key)
		return
	}
	writeXML(w, http.StatusOK, CopyPartResult{
		Xmlns:        xmlns,
		ETag:         `"` + etag + `"`,
		LastModified: s3Time(obj.LastModified),
	})
}

func (s *Server) completeMultipartUpload(w http.ResponseWriter, r *http.Request, rid, key, uploadID string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 10<<20))
	if err != nil {
		writeError(w, r, rid, http.StatusBadRequest, "MalformedXML", "The XML provided was not well-formed.", r.URL.Path)
		return
	}
	var req CompleteMultipartUploadRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		writeError(w, r, rid, http.StatusBadRequest, "MalformedXML", "The XML provided was not well-formed.", r.URL.Path)
		return
	}
	parts := make([]store.Part, len(req.Parts))
	for i, p := range req.Parts {
		parts[i] = store.Part{PartNumber: p.PartNumber, ETag: p.ETag}
	}
	v, err := s.Store.CompleteMultipartUpload(uploadID, parts)
	if err != nil {
		storeError(w, r, rid, err, key)
		return
	}
	if v.VersionID != "" {
		w.Header().Set("x-amz-version-id", v.VersionID)
	}
	location := "http://" + r.Host + "/" + s.Bucket + "/" + key
	writeXML(w, http.StatusOK, CompleteMultipartUploadResult{
		Xmlns:    xmlns,
		Location: location,
		Bucket:   s.Bucket,
		Key:      key,
		ETag:     `"` + v.ETag + `"`,
	})
}

func (s *Server) listParts(w http.ResponseWriter, r *http.Request, rid, key, uploadID string) {
	q := r.URL.Query()
	maxParts := parseMaxKeys(q.Get("max-parts"), 1000)
	if maxParts < 0 {
		writeError(w, r, rid, http.StatusBadRequest, "InvalidArgument", "max-parts is invalid", r.URL.Path)
		return
	}
	marker := atoiDefault(q.Get("part-number-marker"), 0)
	res, err := s.Store.ListParts(uploadID, maxParts, marker)
	if err != nil {
		storeError(w, r, rid, err, key)
		return
	}
	out := ListPartsResult{
		Xmlns:                xmlns,
		Bucket:               s.Bucket,
		Key:                  key,
		UploadID:             uploadID,
		StorageClass:         "STANDARD",
		PartNumberMarker:     marker,
		NextPartNumberMarker: res.NextMarker,
		MaxParts:             maxParts,
		IsTruncated:          res.IsTruncated,
	}
	for _, p := range res.Parts {
		out.Parts = append(out.Parts, PartEntry{
			PartNumber:   p.PartNumber,
			LastModified: s3Time(p.LastModified),
			ETag:         `"` + p.ETag + `"`,
			Size:         p.Size,
		})
	}
	writeXML(w, http.StatusOK, out)
}

func (s *Server) listMultipartUploads(w http.ResponseWriter, r *http.Request, rid string) {
	q := r.URL.Query()
	p := store.ListUploadsParams{
		Prefix:         q.Get("prefix"),
		Delimiter:      q.Get("delimiter"),
		MaxUploads:     parseMaxKeys(q.Get("max-uploads"), 1000),
		KeyMarker:      q.Get("key-marker"),
		UploadIDMarker: q.Get("upload-id-marker"),
	}
	encType := q.Get("encoding-type")
	if p.MaxUploads < 0 {
		writeError(w, r, rid, http.StatusBadRequest, "InvalidArgument", "max-uploads is invalid", r.URL.Path)
		return
	}
	res, err := s.Store.ListMultipartUploads(p)
	if err != nil {
		storeError(w, r, rid, err, "")
		return
	}
	out := ListMultipartUploadsResult{
		Xmlns:              xmlns,
		Bucket:             s.Bucket,
		Prefix:             enc(p.Prefix, encType),
		Delimiter:          enc(p.Delimiter, encType),
		KeyMarker:          p.KeyMarker,
		UploadIDMarker:     p.UploadIDMarker,
		NextKeyMarker:      res.NextKeyMarker,
		NextUploadIDMarker: res.NextUploadIDMarker,
		MaxUploads:         p.MaxUploads,
		IsTruncated:        res.IsTruncated,
		EncodingType:       encType,
	}
	for _, cp := range res.CommonPrefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, CommonPrefix{Prefix: enc(cp, encType)})
	}
	for _, u := range res.Uploads {
		out.Uploads = append(out.Uploads, UploadEntry{
			Key: enc(u.Key, encType), UploadID: u.UploadID,
			Initiated: s3Time(u.Initiated), StorageClass: "STANDARD",
		})
	}
	writeXML(w, http.StatusOK, out)
}
