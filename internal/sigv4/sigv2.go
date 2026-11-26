package sigv4

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SigV2 support (legacy query/header signing), needed because boto3's
// generate_presigned_url defaults to SigV2.

var sigV2Subresources = []string{
	"acl", "cors", "lifecycle", "location", "logging", "notification",
	"partNumber", "policy", "requestPayment", "restore", "tagging",
	"torrent", "uploadId", "uploads", "versionId", "versioning", "versions", "website",
	"delete",
}

func (a *Auth) verifySigV2Query(r *http.Request) error {
	q := r.URL.Query()
	ak := q.Get("AWSAccessKeyId")
	if ak != a.AccessKey {
		return ErrBadCred
	}
	sig := q.Get("Signature")
	expiresStr := q.Get("Expires")
	expires, err := strconv.ParseInt(expiresStr, 10, 64)
	if err != nil {
		return ErrBadAuth
	}
	if time.Now().Unix() > expires {
		return ErrExpired
	}
	sts := r.Method + "\n" +
		r.Header.Get("Content-MD5") + "\n" +
		r.Header.Get("Content-Type") + "\n" +
		expiresStr + "\n" +
		canonicalizedAmzHeadersV2(r) +
		canonicalizedResourceV2(r)
	want := sigV2Sign(a.SecretKey, sts)
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return ErrBadSig
	}
	return nil
}

func (a *Auth) verifySigV2Header(r *http.Request) error {
	auth := r.Header.Get("Authorization")
	parts := strings.SplitN(strings.TrimPrefix(auth, "AWS "), ":", 2)
	if len(parts) != 2 {
		return ErrBadAuth
	}
	if parts[0] != a.AccessKey {
		return ErrBadCred
	}
	date := r.Header.Get("Date")
	if date == "" {
		date = r.Header.Get("X-Amz-Date")
	}
	sts := r.Method + "\n" +
		r.Header.Get("Content-MD5") + "\n" +
		r.Header.Get("Content-Type") + "\n" +
		date + "\n" +
		canonicalizedAmzHeadersV2(r) +
		canonicalizedResourceV2(r)
	want := sigV2Sign(a.SecretKey, sts)
	if !hmac.Equal([]byte(want), []byte(parts[1])) {
		return ErrBadSig
	}
	return nil
}

func sigV2Sign(secret, sts string) string {
	h := hmac.New(sha1.New, []byte(secret))
	h.Write([]byte(sts))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func canonicalizedAmzHeadersV2(r *http.Request) string {
	var headers []string
	for name := range r.Header {
		lname := strings.ToLower(name)
		if strings.HasPrefix(lname, "x-amz-") {
			headers = append(headers, lname+":"+strings.TrimSpace(r.Header.Get(name)))
		}
	}
	// sort
	for i := 0; i < len(headers); i++ {
		for j := i + 1; j < len(headers); j++ {
			if headers[j] < headers[i] {
				headers[i], headers[j] = headers[j], headers[i]
			}
		}
	}
	if len(headers) == 0 {
		return ""
	}
	return strings.Join(headers, "\n") + "\n"
}

func canonicalizedResourceV2(r *http.Request) string {
	res := r.URL.EscapedPath()
	if res == "" {
		res = "/"
	}
	q := r.URL.Query()
	var subs []string
	for _, s := range sigV2Subresources {
		if q.Has(s) {
			subs = append(subs, s+"="+q.Get(s))
		}
	}
	if len(subs) > 0 {
		res += "?" + strings.Join(subs, "&")
	}
	return res
}
