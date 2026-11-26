package store

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	metaFile    = "meta.json"
	dataFile    = "data"
	objectsDir  = "objects"
	versionsDir = ".v" // never produced by encodeSegment (leading '.' is escaped)
	uploadsDir  = "uploads"
	partsDir    = "parts"
	bucketFile  = ".bucket.json"
)

// bucketConfig is the on-disk bucket configuration (shared by all backends).
type bucketConfig struct {
	Name       string    `json:"name"`
	Created    time.Time `json:"created"`
	Versioning string    `json:"versioning"`
}

// versionMeta is the per-version metadata sidecar stored next to the data.
type versionMeta struct {
	VersionID    string    `json:"versionId"`
	Size         int64     `json:"size"`
	ETag         string    `json:"etag"`
	ContentType  string    `json:"contentType"`
	LastModified time.Time `json:"lastModified"`
	DeleteMarker bool      `json:"deleteMarker"`
}

type uploadMeta struct {
	UploadID    string    `json:"uploadId"`
	Key         string    `json:"key"`
	Initiated   time.Time `json:"initiated"`
	ContentType string    `json:"contentType"`
}

type partMeta struct {
	PartNumber int    `json:"partNumber"`
	ETag       string `json:"etag"`
	Size       int64  `json:"size"`
}

// encodeSegment makes a single key segment safe and unique as a directory name.
// The empty segment and any segment starting with '.' get special encodings so
// they can never collide with reserved names (".v", ".bucket.json", ...).
func encodeSegment(s string) string {
	if s == "" {
		return "~empty~"
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	out := b.String()
	if out == versionsDir || out == metaFile || out == dataFile {
		out = "%2E" + out[1:]
	}
	return out
}

func decodeSegment(s string) (string, error) {
	if s == "~empty~" {
		return "", nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' {
			if i+2 >= len(s) {
				return "", fmt.Errorf("bad encoding in %q", s)
			}
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+3], "%02x", &v); err != nil {
				return "", fmt.Errorf("bad encoding in %q", s)
			}
			b.WriteByte(byte(v))
			i += 2
		} else {
			b.WriteByte(s[i])
		}
	}
	return b.String(), nil
}

// ValidateKey rejects keys that cannot be stored safely on a filesystem.
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("key must not be empty")
	}
	if len(key) > 1024 {
		return fmt.Errorf("key too long")
	}
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("key must not start with /")
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == ".." || seg == "." {
			return fmt.Errorf("key contains invalid segment %q", seg)
		}
	}
	return nil
}

func objectDir(root, key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = encodeSegment(p)
	}
	return filepath.Join(append([]string{root, objectsDir}, parts...)...)
}

func versionDir(root, key, versionID string) string {
	return filepath.Join(objectDir(root, key), versionsDir, versionID)
}

func uploadDir(root, uploadID string) string {
	return filepath.Join(root, uploadsDir, uploadID)
}

func partPath(root, uploadID string, partNumber int) string {
	return filepath.Join(uploadDir(root, uploadID), partsDir, fmt.Sprintf("%d.part", partNumber))
}

func partMetaPath(root, uploadID string, partNumber int) string {
	return filepath.Join(uploadDir(root, uploadID), partsDir, fmt.Sprintf("%d.%s", partNumber, metaFile))
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSONAtomic(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadBucketConfig(root, name string) (*bucketConfig, error) {
	path := filepath.Join(root, bucketFile)
	var cfg bucketConfig
	if err := readJSON(path, &cfg); err != nil {
		if os.IsNotExist(err) {
			cfg = bucketConfig{Name: name, Created: time.Now().UTC(), Versioning: VersioningUnversioned}
			if err := writeJSONAtomic(path, &cfg); err != nil {
				return nil, err
			}
			return &cfg, nil
		}
		return nil, err
	}
	return &cfg, nil
}

func saveBucketConfig(root string, cfg *bucketConfig) error {
	return writeJSONAtomic(filepath.Join(root, bucketFile), cfg)
}

// scanVersions walks the object tree and returns all versions grouped by key,
// each group sorted newest first with IsLatest set on the first entry.
func scanVersions(root string) (map[string][]Version, error) {
	out := map[string][]Version{}
	objRoot := filepath.Join(root, objectsDir)
	var walk func(dir, keySoFar string) error
	walk = func(dir, keySoFar string) error {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			if e.Name() == versionsDir {
				vdir := filepath.Join(dir, versionsDir)
				vers, err := os.ReadDir(vdir)
				if err != nil {
					return err
				}
				for _, v := range vers {
					if !v.IsDir() {
						continue
					}
					var m versionMeta
					if err := readJSON(filepath.Join(vdir, v.Name(), metaFile), &m); err != nil {
						continue
					}
					out[keySoFar] = append(out[keySoFar], Version{
						Key:          keySoFar,
						VersionID:    m.VersionID,
						Size:         m.Size,
						ETag:         m.ETag,
						ContentType:  m.ContentType,
						LastModified: m.LastModified,
						DeleteMarker: m.DeleteMarker,
					})
				}
				continue
			}
			seg, err := decodeSegment(e.Name())
			if err != nil {
				continue
			}
			childKey := seg
			if keySoFar != "" {
				childKey = keySoFar + "/" + seg
			}
			if err := walk(filepath.Join(dir, e.Name()), childKey); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(objRoot, ""); err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for key, vers := range out {
		sort.SliceStable(vers, func(i, j int) bool {
			if vers[i].LastModified.Equal(vers[j].LastModified) {
				return vers[i].VersionID > vers[j].VersionID
			}
			return vers[i].LastModified.After(vers[j].LastModified)
		})
		if len(vers) > 0 {
			vers[0].IsLatest = true
		}
		out[key] = vers
	}
	return out, nil
}

func newVersionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)
}
