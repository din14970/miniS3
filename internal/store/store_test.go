package store

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStores(t *testing.T) map[string]Store {
	t.Helper()
	dir := t.TempDir()
	stores := map[string]Store{}
	fs, err := NewFSStore(filepath.Join(dir, "fs"), "b")
	if err != nil {
		t.Fatal(err)
	}
	stores["fs"] = fs
	smem, err := NewSQLiteStore(filepath.Join(dir, "smem"), "b", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	stores["sqlite-memory"] = smem
	sfile, err := NewSQLiteStore(filepath.Join(dir, "sfile"), "b", filepath.Join(dir, "sfile.db"))
	if err != nil {
		t.Fatal(err)
	}
	stores["sqlite-file"] = sfile
	return stores
}

func testAll(t *testing.T, fn func(t *testing.T, s Store)) {
	for name, s := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			defer s.Close()
			fn(t, s)
		})
	}
}

func TestPutGetHead(t *testing.T) {
	testAll(t, func(t *testing.T, s Store) {
		body := []byte("hello world")
		v, err := s.PutObject("a/b.txt", bytes.NewReader(body), int64(len(body)), "text/plain")
		if err != nil {
			t.Fatal(err)
		}
		sum := md5.Sum(body)
		if v.ETag != hex.EncodeToString(sum[:]) {
			t.Fatalf("etag = %s, want %s", v.ETag, hex.EncodeToString(sum[:]))
		}
		if v.VersionID != NullVersionID {
			t.Fatalf("unversioned put should use null version, got %q", v.VersionID)
		}
		obj, err := s.GetObject("a/b.txt", "")
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(obj.Body)
		obj.Close()
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("get = %q err=%v", got, err)
		}
		if obj.ContentType != "text/plain" || obj.Size != int64(len(body)) {
			t.Fatalf("meta = %+v", obj.Version)
		}
		h, err := s.HeadObject("a/b.txt", "")
		if err != nil || h.Size != int64(len(body)) {
			t.Fatalf("head = %+v err=%v", h, err)
		}
		if _, err := s.GetObject("missing", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})
}

func TestListPrefixDelimiterPagination(t *testing.T) {
	testAll(t, func(t *testing.T, s Store) {
		keys := []string{"a/1", "a/2", "a/b/3", "b/1", "c"}
		for _, k := range keys {
			if _, err := s.PutObject(k, strings.NewReader(k), int64(len(k)), ""); err != nil {
				t.Fatal(err)
			}
		}
		res, err := s.ListObjects(ListParams{Prefix: "a/", Delimiter: "/", MaxKeys: 100}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.CommonPrefixes) != 1 || res.CommonPrefixes[0] != "a/b/" {
			t.Fatalf("prefixes = %v", res.CommonPrefixes)
		}
		if len(res.Objects) != 2 || res.Objects[0].Key != "a/1" || res.Objects[1].Key != "a/2" {
			t.Fatalf("objects = %+v", res.Objects)
		}
		// pagination
		res, err = s.ListObjects(ListParams{MaxKeys: 2}, false)
		if err != nil || len(res.Objects) != 2 || !res.IsTruncated {
			t.Fatalf("page1 = %+v err=%v", res, err)
		}
		res2, err := s.ListObjects(ListParams{MaxKeys: 2, ContinuationToken: res.NextContinuationToken}, false)
		if err != nil || len(res2.Objects) != 2 || res2.Objects[0].Key != "a/b/3" || res2.Objects[1].Key != "b/1" {
			t.Fatalf("page2 = %+v err=%v", res2, err)
		}
		// v1 marker
		res3, err := s.ListObjects(ListParams{MaxKeys: 3, Marker: "a/2"}, false)
		if err != nil || len(res3.Objects) != 3 || res3.Objects[0].Key != "a/b/3" {
			t.Fatalf("marker page = %+v err=%v", res3, err)
		}
	})
}

func TestVersioning(t *testing.T) {
	testAll(t, func(t *testing.T, s Store) {
		if err := s.PutVersioning(VersioningEnabled); err != nil {
			t.Fatal(err)
		}
		if s.GetVersioning() != VersioningEnabled {
			t.Fatal("versioning not enabled")
		}
		var vids []string
		for i := 0; i < 3; i++ {
			v, err := s.PutObject("k", strings.NewReader(fmt.Sprintf("v%d", i)), 2, "")
			if err != nil {
				t.Fatal(err)
			}
			if v.VersionID == NullVersionID {
				t.Fatal("versioned bucket should not use null id")
			}
			vids = append(vids, v.VersionID)
		}
		// latest is v2
		obj, err := s.GetObject("k", "")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(obj.Body)
		obj.Close()
		if string(b) != "v2" {
			t.Fatalf("latest = %q", b)
		}
		// get specific version
		obj, err = s.GetObject("k", vids[0])
		if err != nil {
			t.Fatal(err)
		}
		b, _ = io.ReadAll(obj.Body)
		obj.Close()
		if string(b) != "v0" {
			t.Fatalf("v0 = %q", b)
		}
		// versions listing
		res, err := s.ListObjects(ListParams{MaxKeys: 100}, true)
		if err != nil || len(res.Objects) != 3 {
			t.Fatalf("versions = %+v err=%v", res, err)
		}
		if res.Objects[0].VersionID != vids[2] || !res.Objects[0].IsLatest {
			t.Fatalf("newest first expected, got %+v", res.Objects)
		}
		// delete without version -> delete marker
		dm, err := s.DeleteObject("k", "")
		if err != nil || dm == nil || !dm.DeleteMarker {
			t.Fatalf("delete marker = %+v err=%v", dm, err)
		}
		if _, err := s.GetObject("k", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("after marker, get should 404, got %v", err)
		}
		res, _ = s.ListObjects(ListParams{MaxKeys: 100}, true)
		if len(res.Objects) != 4 || !res.Objects[0].DeleteMarker {
			t.Fatalf("versions after marker = %+v", res.Objects)
		}
		// delete the marker -> latest visible again
		if _, err := s.DeleteObject("k", dm.VersionID); err != nil {
			t.Fatal(err)
		}
		obj, err = s.GetObject("k", "")
		if err != nil {
			t.Fatal(err)
		}
		b, _ = io.ReadAll(obj.Body)
		obj.Close()
		if string(b) != "v2" {
			t.Fatalf("after marker removal = %q", b)
		}
		// delete a specific version
		if _, err := s.DeleteObject("k", vids[1]); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetObject("k", vids[1]); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted version should be gone, got %v", err)
		}
		// suspended -> null version overwrite
		if err := s.PutVersioning(VersioningSuspended); err != nil {
			t.Fatal(err)
		}
		v, err := s.PutObject("k", strings.NewReader("new"), 3, "")
		if err != nil || v.VersionID != NullVersionID {
			t.Fatalf("suspended put = %+v err=%v", v, err)
		}
		res, _ = s.ListObjects(ListParams{MaxKeys: 100}, true)
		var nullCount int
		for _, vv := range res.Objects {
			if vv.Key == "k" && vv.VersionID == NullVersionID {
				nullCount++
			}
		}
		if nullCount != 1 {
			t.Fatalf("want single null version, versions=%+v", res.Objects)
		}
	})
}

func TestCopy(t *testing.T) {
	testAll(t, func(t *testing.T, s Store) {
		if _, err := s.PutObject("src", strings.NewReader("data"), 4, "text/plain"); err != nil {
			t.Fatal(err)
		}
		v, err := s.CopyObject("dst", "src", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if v.ContentType != "text/plain" {
			t.Fatalf("copied content type = %q", v.ContentType)
		}
		obj, err := s.GetObject("dst", "")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(obj.Body)
		obj.Close()
		if string(b) != "data" {
			t.Fatalf("copy = %q", b)
		}
	})
}

func TestMultipart(t *testing.T) {
	testAll(t, func(t *testing.T, s Store) {
		uid, err := s.CreateMultipartUpload("mp/obj", "application/octet-stream")
		if err != nil {
			t.Fatal(err)
		}
		part1 := bytes.Repeat([]byte("a"), 5<<20+10) // >= 5MiB
		part2 := []byte("bb")
		e1, err := s.UploadPart(uid, 1, bytes.NewReader(part1), int64(len(part1)))
		if err != nil {
			t.Fatal(err)
		}
		e2, err := s.UploadPart(uid, 2, bytes.NewReader(part2), int64(len(part2)))
		if err != nil {
			t.Fatal(err)
		}
		lp, err := s.ListParts(uid, 100, 0)
		if err != nil || len(lp.Parts) != 2 || lp.Parts[0].PartNumber != 1 {
			t.Fatalf("parts = %+v err=%v", lp, err)
		}
		v, err := s.CompleteMultipartUpload(uid, []Part{{1, e1}, {2, e2}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(v.ETag, "-2") {
			t.Fatalf("multipart etag = %q", v.ETag)
		}
		if v.Size != int64(len(part1)+len(part2)) {
			t.Fatalf("size = %d", v.Size)
		}
		obj, err := s.GetObject("mp/obj", "")
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(obj.Body)
		obj.Close()
		if !bytes.Equal(got, append(part1, part2...)) {
			t.Fatal("multipart content mismatch")
		}
		// uploads list should be empty now
		lu, err := s.ListMultipartUploads(ListUploadsParams{MaxUploads: 100})
		if err != nil || len(lu.Uploads) != 0 {
			t.Fatalf("uploads = %+v err=%v", lu, err)
		}
		// abort flow
		uid2, _ := s.CreateMultipartUpload("mp/aborted", "")
		lu, _ = s.ListMultipartUploads(ListUploadsParams{MaxUploads: 100})
		if len(lu.Uploads) != 1 || lu.Uploads[0].Key != "mp/aborted" {
			t.Fatalf("uploads = %+v", lu.Uploads)
		}
		if err := s.AbortMultipartUpload(uid2); err != nil {
			t.Fatal(err)
		}
		if err := s.AbortMultipartUpload(uid2); !errors.Is(err, ErrNoSuchUpload) {
			t.Fatalf("double abort err = %v", err)
		}
		// too-small non-final part
		uid3, _ := s.CreateMultipartUpload("mp/small", "")
		e, _ := s.UploadPart(uid3, 1, strings.NewReader("tiny"), 4)
		_, err = s.CompleteMultipartUpload(uid3, []Part{{1, e}, {2, "x"}})
		if !errors.Is(err, ErrPartTooSmall) && !errors.Is(err, ErrInvalidPart) {
			t.Fatalf("want ErrPartTooSmall/ErrInvalidPart, got %v", err)
		}
		s.AbortMultipartUpload(uid3)
	})
}

func TestBucketLifecycle(t *testing.T) {
	testAll(t, func(t *testing.T, s Store) {
		if !s.Exists() {
			t.Fatal("bucket should exist after open")
		}
		if _, err := s.PutObject("x", strings.NewReader("1"), 1, ""); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteBucket(); !errors.Is(err, ErrBucketNotEmpty) {
			t.Fatalf("want BucketNotEmpty, got %v", err)
		}
		if _, err := s.DeleteObject("x", ""); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteBucket(); err != nil {
			t.Fatal(err)
		}
		if s.Exists() {
			t.Fatal("bucket should be gone")
		}
		if err := s.CreateBucket(); err != nil {
			t.Fatal(err)
		}
		if !s.Exists() {
			t.Fatal("bucket should be back")
		}
	})
}

// TestAutoPopulate verifies that an empty SQLite DB populates itself from the
// data directory written by the DB-less backend.
func TestAutoPopulate(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "b")
	fs, err := NewFSStore(root, "b")
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.PutVersioning(VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.PutObject("dir/hello.txt", strings.NewReader("hi"), 2, "text/plain"); err != nil {
		t.Fatal(err)
	}
	v2, err := fs.PutObject("dir/hello.txt", strings.NewReader("hi2"), 3, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.PutObject("other.bin", bytes.NewReader(bytes.Repeat([]byte{7}, 100)), 100, ""); err != nil {
		t.Fatal(err)
	}
	fs.Close()

	for _, dsn := range []string{":memory:", filepath.Join(dir, "meta.db")} {
		s, err := NewSQLiteStore(root, "b", dsn)
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.ListObjects(ListParams{MaxKeys: 100}, false)
		if err != nil || len(res.Objects) != 2 {
			t.Fatalf("dsn=%s objects = %+v err=%v", dsn, res, err)
		}
		obj, err := s.GetObject("dir/hello.txt", "")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(obj.Body)
		obj.Close()
		if string(b) != "hi2" {
			t.Fatalf("dsn=%s latest = %q", dsn, b)
		}
		// old version still retrievable
		res2, err := s.ListObjects(ListParams{MaxKeys: 100}, true)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, vv := range res2.Objects {
			if vv.Key == "dir/hello.txt" && vv.VersionID != v2.VersionID {
				found = true
			}
		}
		if !found {
			t.Fatalf("dsn=%s old version missing: %+v", dsn, res2.Objects)
		}
		if s.GetVersioning() != VersioningEnabled {
			t.Fatalf("dsn=%s versioning not persisted", dsn)
		}
		s.Close()
	}
}

func TestKeyValidationAndWeirdKeys(t *testing.T) {
	testAll(t, func(t *testing.T, s Store) {
		for _, bad := range []string{"", "/abs", "a/../b", "a/./b"} {
			if err := ValidateKey(bad); err == nil {
				t.Fatalf("key %q should be rejected", bad)
			}
		}
		weird := []string{"with space.txt", "üñïçødé.txt", "a//b", "trailing/", "sym?bols&=%2F.txt", ".v", ".hidden", "meta.json"}
		for _, k := range weird {
			if err := ValidateKey(k); err != nil {
				t.Fatalf("key %q rejected: %v", k, err)
			}
			if _, err := s.PutObject(k, strings.NewReader(k), int64(len(k)), ""); err != nil {
				t.Fatalf("put %q: %v", k, err)
			}
		}
		res, err := s.ListObjects(ListParams{MaxKeys: 100}, false)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, v := range res.Objects {
			got[v.Key] = true
		}
		for _, k := range weird {
			if !got[k] {
				t.Fatalf("key %q missing from listing: %v", k, got)
			}
			obj, err := s.GetObject(k, "")
			if err != nil {
				t.Fatalf("get %q: %v", k, err)
			}
			b, _ := io.ReadAll(obj.Body)
			obj.Close()
			if string(b) != k {
				t.Fatalf("get %q = %q", k, b)
			}
		}
	})
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
