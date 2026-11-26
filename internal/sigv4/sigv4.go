// Package sigv4 implements AWS Signature Version 4 verification for the S3 API.
package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Auth verifies SigV4 requests. When AccessKey is empty, Verify is a no-op
// (permissive development mode).
type Auth struct {
	AccessKey string
	SecretKey string
}

func (a *Auth) Enabled() bool { return a != nil && a.AccessKey != "" }

var (
	ErrNoAuth      = errors.New("missing authorization")
	ErrBadAuth     = errors.New("malformed authorization header")
	ErrBadCred     = errors.New("unknown access key")
	ErrBadSig      = errors.New("signature does not match")
	ErrExpired     = errors.New("presigned URL expired")
	ErrUnsupported = errors.New("unsupported signing payload")
)

const algorithm = "AWS4-HMAC-SHA256"

// Verify validates the request signature. payloadHash is the payload hash the
// client signed (for header auth this is the X-Amz-Content-Sha256 header
// value; for presigned query auth it is UNSIGNED-PAYLOAD).
func (a *Auth) Verify(r *http.Request, payloadHash string) error {
	if !a.Enabled() {
		return nil
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		if strings.HasPrefix(auth, "AWS ") {
			return a.verifySigV2Header(r)
		}
		return a.verifyHeader(r, auth, payloadHash)
	}
	q := r.URL.Query()
	if q.Get("X-Amz-Algorithm") == algorithm {
		return a.verifyQuery(r, payloadHash)
	}
	if q.Get("AWSAccessKeyId") != "" && q.Get("Signature") != "" {
		return a.verifySigV2Query(r)
	}
	return ErrNoAuth
}

func (a *Auth) verifyHeader(r *http.Request, auth, payloadHash string) error {
	if !strings.HasPrefix(auth, algorithm+" ") {
		return ErrBadAuth
	}
	var credential, signedHeaders, signature string
	for _, part := range strings.Split(auth[len(algorithm)+1:], ",") {
		part = strings.TrimSpace(part)
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return ErrBadAuth
		}
		switch kv[0] {
		case "Credential":
			credential = kv[1]
		case "SignedHeaders":
			signedHeaders = kv[1]
		case "Signature":
			signature = kv[1]
		}
	}
	if credential == "" || signedHeaders == "" || signature == "" {
		return ErrBadAuth
	}
	credParts := strings.Split(credential, "/")
	if len(credParts) != 5 || credParts[0] != a.AccessKey {
		return ErrBadCred
	}
	date, region, service, term := credParts[1], credParts[2], credParts[3], credParts[4]
	if term != "aws4_request" {
		return ErrBadAuth
	}
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		if d := r.Header.Get("Date"); d != "" {
			if t, err := http.ParseTime(d); err == nil {
				amzDate = t.UTC().Format("20060102T150405Z")
			}
		}
	}
	if amzDate == "" {
		return ErrBadAuth
	}
	if len(amzDate) >= 8 && amzDate[:8] != date {
		return ErrBadSig
	}
	if t, err := time.Parse("20060102T150405Z", amzDate); err == nil {
		if d := time.Since(t); d > 15*time.Minute || d < -15*time.Minute {
			return ErrExpired
		}
	}
	cr := canonicalRequest(r, signedHeaders, payloadHash, false)
	return a.checkSignature(cr, amzDate, date, region, service, signature)
}

func (a *Auth) verifyQuery(r *http.Request, payloadHash string) error {
	q := r.URL.Query()
	cred := q.Get("X-Amz-Credential")
	credParts := strings.Split(cred, "/")
	if len(credParts) != 5 || credParts[0] != a.AccessKey {
		return ErrBadCred
	}
	date, region, service, term := credParts[1], credParts[2], credParts[3], credParts[4]
	if term != "aws4_request" {
		return ErrBadAuth
	}
	amzDate := q.Get("X-Amz-Date")
	signature := q.Get("X-Amz-Signature")
	signedHeaders := q.Get("X-Amz-SignedHeaders")
	if amzDate == "" || signature == "" || signedHeaders == "" {
		return ErrBadAuth
	}
	t, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return ErrBadAuth
	}
	expires := int64(0)
	if e := q.Get("X-Amz-Expires"); e != "" {
		fmt.Sscanf(e, "%d", &expires)
	}
	now := time.Now().UTC()
	if now.After(t.Add(time.Duration(expires) * time.Second)) {
		return ErrExpired
	}
	if now.Before(t.Add(-15 * time.Minute)) {
		return ErrExpired
	}
	ph := payloadHash
	if ph == "" {
		ph = "UNSIGNED-PAYLOAD"
	}
	cr := canonicalRequest(r, signedHeaders, ph, true)
	return a.checkSignature(cr, amzDate, date, region, service, signature)
}

func (a *Auth) checkSignature(canonicalReq, amzDate, date, region, service, signature string) error {
	scope := date + "/" + region + "/" + service + "/aws4_request"
	h := sha256.Sum256([]byte(canonicalReq))
	sts := algorithm + "\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(h[:])
	kDate := hmacSHA256([]byte("AWS4"+a.SecretKey), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	kSigning := hmacSHA256(kService, "aws4_request")
	want := hex.EncodeToString(hmacSHA256(kSigning, sts))
	if !hmac.Equal([]byte(want), []byte(strings.ToLower(signature))) {
		return ErrBadSig
	}
	return nil
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func canonicalRequest(r *http.Request, signedHeaders, payloadHash string, queryAuth bool) string {
	var b strings.Builder
	b.WriteString(r.Method)
	b.WriteByte('\n')
	b.WriteString(canonicalURI(r))
	b.WriteByte('\n')
	b.WriteString(canonicalQuery(r, queryAuth))
	b.WriteByte('\n')
	b.WriteString(canonicalHeaders(r, signedHeaders))
	b.WriteByte('\n')
	b.WriteString(signedHeaders)
	b.WriteByte('\n')
	b.WriteString(payloadHash)
	return b.String()
}

func canonicalURI(r *http.Request) string {
	p := r.URL.EscapedPath()
	if p == "" {
		return "/"
	}
	return p
}

func canonicalQuery(r *http.Request, excludeSignature bool) string {
	raw := r.URL.RawQuery
	if raw == "" {
		return ""
	}
	type kv struct{ k, v string }
	var pairs []kv
	for _, part := range strings.Split(raw, "&") {
		k, v := part, ""
		if i := strings.IndexByte(part, '='); i >= 0 {
			k, v = part[:i], part[i+1:]
		}
		dk, err1 := url.QueryUnescape(k)
		dv, err2 := url.QueryUnescape(v)
		if err1 != nil {
			dk = k
		}
		if err2 != nil {
			dv = v
		}
		if excludeSignature && dk == "X-Amz-Signature" {
			continue
		}
		pairs = append(pairs, kv{rfc3986Encode(dk), rfc3986Encode(dv)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		if p.v == "" && !strings.Contains(raw, p.k+"=") {
			parts[i] = p.k
		} else {
			parts[i] = p.k + "=" + p.v
		}
	}
	return strings.Join(parts, "&")
}

func canonicalHeaders(r *http.Request, signedHeaders string) string {
	var b strings.Builder
	for _, name := range strings.Split(signedHeaders, ";") {
		name = strings.ToLower(strings.TrimSpace(name))
		var val string
		if name == "host" {
			val = r.Host
		} else {
			val = r.Header.Get(name)
		}
		val = strings.Join(strings.Fields(val), " ")
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(val)
		b.WriteByte('\n')
	}
	return b.String()
}

func rfc3986Encode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
