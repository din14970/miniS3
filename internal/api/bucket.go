package api

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/din14970/miniS3/internal/store"
)

func s3Time(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func httpTime(t time.Time) string { return t.UTC().Format(http.TimeFormat) }

func enc(s string, encodingType string) string {
	if encodingType == "url" {
		return url.PathEscape(s)
	}
	return s
}

func (s *Server) listBuckets(w http.ResponseWriter, r *http.Request, rid string) {
	res := ListAllMyBucketsResult{
		Xmlns: xmlns,
		Owner: Owner{ID: "minis3", DisplayName: "minis3"},
	}
	if s.Store.Exists() {
		res.Buckets.Bucket = append(res.Buckets.Bucket, BucketEntry{
			Name:         s.Bucket,
			CreationDate: s3Time(s.Store.Created()),
		})
	}
	writeXML(w, http.StatusOK, res)
}

func (s *Server) handleBucket(w http.ResponseWriter, r *http.Request, rid string) {
	q := r.URL.Query()
	switch r.Method {
	case http.MethodHead:
		w.WriteHeader(http.StatusOK)
	case http.MethodPut:
		switch {
		case q.Has("versioning"):
			s.putBucketVersioning(w, r, rid)
		case q.Has("acl"):
			w.WriteHeader(http.StatusOK)
		default:
			if err := s.Store.CreateBucket(); err != nil {
				storeError(w, r, rid, err, "")
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	case http.MethodDelete:
		if err := s.Store.DeleteBucket(); err != nil {
			storeError(w, r, rid, err, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		switch {
		case q.Has("location"):
			writeXML(w, http.StatusOK, LocationConstraint{Xmlns: xmlns, Value: s.Region})
		case q.Has("versioning"):
			status := s.Store.GetVersioning()
			writeXML(w, http.StatusOK, VersioningConfiguration{Xmlns: xmlns, Status: status})
		case q.Has("uploads"):
			s.listMultipartUploads(w, r, rid)
		case q.Has("versions"):
			s.listObjectVersions(w, r, rid)
		case q.Has("acl"):
			s.writeACL(w)
		default:
			if q.Get("list-type") == "2" {
				s.listObjectsV2(w, r, rid)
			} else {
				s.listObjectsV1(w, r, rid)
			}
		}
	case http.MethodPost:
		if q.Has("delete") {
			s.deleteObjects(w, r, rid)
			return
		}
		writeError(w, r, rid, http.StatusMethodNotAllowed, "MethodNotAllowed", "method not allowed", r.URL.Path)
	default:
		writeError(w, r, rid, http.StatusMethodNotAllowed, "MethodNotAllowed", "method not allowed", r.URL.Path)
	}
}

func (s *Server) putBucketVersioning(w http.ResponseWriter, r *http.Request, rid string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, r, rid, http.StatusBadRequest, "MalformedXML", "The XML provided was not well-formed.", r.URL.Path)
		return
	}
	var cfg VersioningConfiguration
	if err := xml.Unmarshal(body, &cfg); err != nil {
		writeError(w, r, rid, http.StatusBadRequest, "MalformedXML", "The XML provided was not well-formed.", r.URL.Path)
		return
	}
	if err := s.Store.PutVersioning(cfg.Status); err != nil {
		writeError(w, r, rid, http.StatusBadRequest, "InvalidArgument", err.Error(), r.URL.Path)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) writeACL(w http.ResponseWriter) {
	acl := ACL{Xmlns: xmlns, Owner: Owner{ID: "minis3", DisplayName: "minis3"}}
	g := struct {
		Grantee struct {
			Type        string `xml:"xsi:type,attr"`
			ID          string `xml:"ID"`
			DisplayName string `xml:"DisplayName"`
		} `xml:"Grantee"`
		Permission string `xml:"Permission"`
	}{}
	g.Grantee.Type = "CanonicalUser"
	g.Grantee.ID = "minis3"
	g.Grantee.DisplayName = "minis3"
	g.Permission = "FULL_CONTROL"
	acl.Grants.Grant = append(acl.Grants.Grant, g)
	writeXML(w, http.StatusOK, acl)
}

func (s *Server) listParams(r *http.Request, versions bool) (store.ListParams, int, string, bool) {
	q := r.URL.Query()
	p := store.ListParams{
		Prefix:    q.Get("prefix"),
		Delimiter: q.Get("delimiter"),
		MaxKeys:   parseMaxKeys(q.Get("max-keys"), 1000),
	}
	if p.MaxKeys < 0 {
		p.MaxKeys = -1
	}
	p.StartAfter = q.Get("start-after")
	p.ContinuationToken = q.Get("continuation-token")
	p.Marker = q.Get("marker")
	p.KeyMarker = q.Get("key-marker")
	p.VersionIDMarker = q.Get("version-id-marker")
	return p, p.MaxKeys, q.Get("encoding-type"), q.Get("list-type") == "2"
}

func (s *Server) listObjectsV2(w http.ResponseWriter, r *http.Request, rid string) {
	p, maxKeys, encType, _ := s.listParams(r, false)
	if maxKeys < 0 {
		writeError(w, r, rid, http.StatusBadRequest, "InvalidArgument", "max-keys is invalid", r.URL.Path)
		return
	}
	res, err := s.Store.ListObjects(p, false)
	if err != nil {
		storeError(w, r, rid, err, "")
		return
	}
	out := ListBucketResult{
		Xmlns:                 xmlns,
		Name:                  s.Bucket,
		Prefix:                enc(p.Prefix, encType),
		MaxKeys:               maxKeys,
		IsTruncated:           res.IsTruncated,
		KeyCount:              len(res.Objects) + len(res.CommonPrefixes),
		Delimiter:             enc(p.Delimiter, encType),
		StartAfter:            enc(p.StartAfter, encType),
		ContinuationToken:     p.ContinuationToken,
		NextContinuationToken: res.NextContinuationToken,
		EncodingType:          encType,
	}
	for _, cp := range res.CommonPrefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, CommonPrefix{Prefix: enc(cp, encType)})
	}
	for _, v := range res.Objects {
		out.Contents = append(out.Contents, Contents{
			Key:          enc(v.Key, encType),
			LastModified: s3Time(v.LastModified),
			ETag:         `"` + v.ETag + `"`,
			Size:         v.Size,
			StorageClass: "STANDARD",
		})
	}
	writeXML(w, http.StatusOK, out)
}

func (s *Server) listObjectsV1(w http.ResponseWriter, r *http.Request, rid string) {
	p, maxKeys, encType, _ := s.listParams(r, false)
	if maxKeys < 0 {
		writeError(w, r, rid, http.StatusBadRequest, "InvalidArgument", "max-keys is invalid", r.URL.Path)
		return
	}
	res, err := s.Store.ListObjects(p, false)
	if err != nil {
		storeError(w, r, rid, err, "")
		return
	}
	out := ListBucketResult{
		Xmlns:       xmlns,
		Name:        s.Bucket,
		Prefix:      enc(p.Prefix, encType),
		Marker:       p.Marker,
		NextMarker:   res.NextMarker,
		MaxKeys:      maxKeys,
		IsTruncated:  res.IsTruncated,
		Delimiter:    enc(p.Delimiter, encType),
		EncodingType: encType,
	}
	for _, cp := range res.CommonPrefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, CommonPrefix{Prefix: enc(cp, encType)})
	}
	for _, v := range res.Objects {
		out.Contents = append(out.Contents, Contents{
			Key:          enc(v.Key, encType),
			LastModified: s3Time(v.LastModified),
			ETag:         `"` + v.ETag + `"`,
			Size:         v.Size,
			StorageClass: "STANDARD",
		})
	}
	writeXML(w, http.StatusOK, out)
}

func (s *Server) listObjectVersions(w http.ResponseWriter, r *http.Request, rid string) {
	p, maxKeys, encType, _ := s.listParams(r, true)
	if maxKeys < 0 {
		writeError(w, r, rid, http.StatusBadRequest, "InvalidArgument", "max-keys is invalid", r.URL.Path)
		return
	}
	res, err := s.Store.ListObjects(p, true)
	if err != nil {
		storeError(w, r, rid, err, "")
		return
	}
	out := ListVersionsResult{
		Xmlns:               xmlns,
		Name:                s.Bucket,
		Prefix:              enc(p.Prefix, encType),
		KeyMarker:           p.KeyMarker,
		VersionIDMarker:     p.VersionIDMarker,
		NextKeyMarker:       res.NextKeyMarker,
		NextVersionIDMarker: res.NextVersionIDMarker,
		MaxKeys:             maxKeys,
		Delimiter:           enc(p.Delimiter, encType),
		IsTruncated:         res.IsTruncated,
		EncodingType:        encType,
	}
	for _, cp := range res.CommonPrefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, CommonPrefix{Prefix: enc(cp, encType)})
	}
	for _, v := range res.Objects {
		if v.DeleteMarker {
			out.DeleteMarkers = append(out.DeleteMarkers, DeleteMarkerEntry{
				Key: enc(v.Key, encType), VersionID: v.VersionID, IsLatest: v.IsLatest,
				LastModified: s3Time(v.LastModified),
			})
		} else {
			out.Versions = append(out.Versions, VersionEntry{
				Key: enc(v.Key, encType), VersionID: v.VersionID, IsLatest: v.IsLatest,
				LastModified: s3Time(v.LastModified), ETag: `"` + v.ETag + `"`,
				Size: v.Size, StorageClass: "STANDARD",
			})
		}
	}
	writeXML(w, http.StatusOK, out)
}

func (s *Server) deleteObjects(w http.ResponseWriter, r *http.Request, rid string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 10<<20))
	if err != nil {
		writeError(w, r, rid, http.StatusBadRequest, "MalformedXML", "The XML provided was not well-formed.", r.URL.Path)
		return
	}
	var req DeleteRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		writeError(w, r, rid, http.StatusBadRequest, "MalformedXML", "The XML provided was not well-formed.", r.URL.Path)
		return
	}
	if len(req.Objects) > 1000 {
		writeError(w, r, rid, http.StatusBadRequest, "MalformedXML", "The XML provided contains more than 1000 objects.", r.URL.Path)
		return
	}
	res := DeleteResult{Xmlns: xmlns}
	for _, obj := range req.Objects {
		v, err := s.Store.DeleteObject(obj.Key, obj.VersionID)
		if err != nil {
			res.Errors = append(res.Errors, DeleteErrorEntry{
				Key: obj.Key, VersionID: obj.VersionID, Code: "InternalError", Message: err.Error(),
			})
			continue
		}
		d := DeletedEntry{Key: obj.Key}
		if v != nil {
			d.VersionID = v.VersionID
			d.DeleteMarker = v.DeleteMarker
		} else if obj.VersionID != "" {
			d.VersionID = obj.VersionID
		}
		res.Deleted = append(res.Deleted, d)
	}
	writeXML(w, http.StatusOK, res)
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
