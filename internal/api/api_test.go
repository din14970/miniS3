package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/din14970/miniS3/internal/sigv4"
	"github.com/din14970/miniS3/internal/store"
)

func TestAPIObjectLifecycle(t *testing.T) {
	st, err := store.NewFSStore(filepath.Join(t.TempDir(), "b"), "b")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Store: st, Bucket: "b", Region: "us-east-1", Auth: &sigv4.Auth{}}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// PUT object
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/b/dir/hello.txt", strings.NewReader("hello"))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("ETag") == "" {
		t.Fatalf("put: %d etag=%q", resp.StatusCode, resp.Header.Get("ETag"))
	}

	// GET object
	resp, err = http.Get(ts.URL + "/b/dir/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "hello" || resp.Header.Get("Content-Type") != "text/plain" {
		t.Fatalf("get: %d %q ct=%q", resp.StatusCode, b, resp.Header.Get("Content-Type"))
	}

	// range
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/b/dir/hello.txt", nil)
	req.Header.Set("Range", "bytes=1-3")
	resp, _ = http.DefaultClient.Do(req)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(b) != "ell" || resp.Header.Get("Content-Range") != "bytes 1-3/5" {
		t.Fatalf("range: %d %q cr=%q", resp.StatusCode, b, resp.Header.Get("Content-Range"))
	}

	// list v2
	resp, _ = http.Get(ts.URL + "/b?list-type=2&prefix=dir/")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "<Key>dir/hello.txt</Key>") || !strings.Contains(string(b), "ListBucketResult") {
		t.Fatalf("list v2 xml: %s", b)
	}

	// list versions on unversioned bucket
	resp, _ = http.Get(ts.URL + "/b?versions")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "<VersionId>null</VersionId>") {
		t.Fatalf("versions xml: %s", b)
	}

	// delete
	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/b/dir/hello.txt", nil)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}

	// get after delete -> NoSuchKey XML
	resp, _ = http.Get(ts.URL + "/b/dir/hello.txt")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 404 || !strings.Contains(string(b), "<Code>NoSuchKey</Code>") {
		t.Fatalf("get after delete: %d %s", resp.StatusCode, b)
	}

	// wrong bucket
	resp, _ = http.Get(ts.URL + "/other?list-type=2")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 404 || !strings.Contains(string(b), "NoSuchBucket") {
		t.Fatalf("wrong bucket: %d %s", resp.StatusCode, b)
	}

	// batch delete via POST ?delete
	st.PutObject("x1", strings.NewReader("1"), 1, "")
	st.PutObject("x2", strings.NewReader("2"), 1, "")
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/b?delete", strings.NewReader(
		`<Delete><Object><Key>x1</Key></Object><Object><Key>x2</Key></Object></Delete>`))
	req.Header.Set("Content-Type", "application/xml")
	resp, _ = http.DefaultClient.Do(req)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "<Deleted>") {
		t.Fatalf("delete objects: %d %s", resp.StatusCode, b)
	}
}

func TestAPIVersioningFlow(t *testing.T) {
	st, err := store.NewFSStore(filepath.Join(t.TempDir(), "b"), "b")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Store: st, Bucket: "b", Region: "us-east-1", Auth: &sigv4.Auth{}}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/b?versioning", strings.NewReader(
		`<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("put versioning: %d", resp.StatusCode)
	}

	var vids []string
	for _, body := range []string{"one", "two"} {
		req, _ = http.NewRequest(http.MethodPut, ts.URL+"/b/k", strings.NewReader(body))
		resp, _ := http.DefaultClient.Do(req)
		vid := resp.Header.Get("x-amz-version-id")
		resp.Body.Close()
		if vid == "" || vid == "null" {
			t.Fatalf("expected version id, got %q", vid)
		}
		vids = append(vids, vid)
	}

	// delete latest -> delete marker
	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/b/k", nil)
	resp, _ = http.DefaultClient.Do(req)
	dm := resp.Header.Get("x-amz-delete-marker")
	dvid := resp.Header.Get("x-amz-version-id")
	resp.Body.Close()
	if resp.StatusCode != 204 || dm != "true" || dvid == "" {
		t.Fatalf("delete marker: %d dm=%q vid=%q", resp.StatusCode, dm, dvid)
	}

	// GET without version -> 404
	resp, _ = http.Get(ts.URL + "/b/k")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("get after marker: %d", resp.StatusCode)
	}

	// GET specific old version -> 200 "one"
	resp, _ = http.Get(ts.URL + "/b/k?versionId=" + vids[0])
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "one" {
		t.Fatalf("get version: %d %q", resp.StatusCode, b)
	}

	// GET delete marker version -> 405
	resp, _ = http.Get(ts.URL + "/b/k?versionId=" + dvid)
	resp.Body.Close()
	if resp.StatusCode != 405 || resp.Header.Get("x-amz-delete-marker") != "true" {
		t.Fatalf("get marker: %d", resp.StatusCode)
	}
}
