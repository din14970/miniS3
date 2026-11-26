package store

import (
	"encoding/base64"
	"sort"
	"strings"
)

// Opaque pagination tokens: "b64:" + base64url(key). Using a prefixed encoding
// keeps tokens stable under encoding-type=url round trips (clients do not
// decode tokens) while still allowing plain keys as markers.
const tokenPrefix = "b64:"

func tokenFor(key string) string {
	return tokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(key))
}

func untoken(s string) string {
	if strings.HasPrefix(s, tokenPrefix) {
		if b, err := base64.RawURLEncoding.DecodeString(s[len(tokenPrefix):]); err == nil {
			return string(b)
		}
	}
	return s
}

// applyListing filters and paginates an already-sorted list of versions
// (sorted by key asc; for version listings, newest first within a key)
// according to S3 list semantics. When versions is false the caller passes
// only the latest non-delete-marker version of each key.
func applyListing(all []Version, versions bool, p ListParams) *ListResult {
	maxKeys := p.MaxKeys
	if maxKeys <= 0 || maxKeys > 1000 {
		maxKeys = 1000
	}

	start := 0
	if versions {
		km := untoken(p.KeyMarker)
		if km != "" {
			start = len(all)
			for i, v := range all {
				if v.Key > km {
					start = i
					break
				}
				if v.Key == km && v.VersionID == p.VersionIDMarker {
					start = i + 1
					break
				}
			}
		}
	} else {
		resume := p.StartAfter
		if p.ContinuationToken != "" {
			resume = untoken(p.ContinuationToken)
		}
		if p.Marker != "" {
			resume = untoken(p.Marker)
		}
		if resume != "" {
			start = len(all)
			for i, v := range all {
				if v.Key > resume {
					start = i
					break
				}
			}
		}
	}

	res := &ListResult{}
	prefixes := map[string]bool{}
	count := 0
	for _, v := range all[start:] {
		if p.Prefix != "" && !hasPrefix(v.Key, p.Prefix) {
			continue
		}
		if p.Delimiter != "" {
			rest := v.Key[len(p.Prefix):]
			if idx := indexOf(rest, p.Delimiter); idx >= 0 {
				cp := p.Prefix + rest[:idx+len(p.Delimiter)]
				if !prefixes[cp] {
					prefixes[cp] = true
					res.CommonPrefixes = append(res.CommonPrefixes, cp)
				}
				continue
			}
		}
		res.Objects = append(res.Objects, v)
		count++
		if count >= maxKeys {
			res.IsTruncated = true
			last := res.Objects[len(res.Objects)-1]
			res.NextMarker = tokenFor(last.Key)
			res.NextContinuationToken = tokenFor(last.Key)
			res.NextKeyMarker = tokenFor(last.Key)
			res.NextVersionIDMarker = last.VersionID
			break
		}
	}
	sort.Strings(res.CommonPrefixes)
	return res
}

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// sortVersionsForListing sorts by key asc; within a key, newest first.
func sortVersionsForListing(vs []Version) {
	sort.SliceStable(vs, func(i, j int) bool {
		if vs[i].Key != vs[j].Key {
			return vs[i].Key < vs[j].Key
		}
		if vs[i].LastModified.Equal(vs[j].LastModified) {
			return vs[i].VersionID > vs[j].VersionID
		}
		return vs[i].LastModified.After(vs[j].LastModified)
	})
}
