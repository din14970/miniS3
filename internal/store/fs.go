package store

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// diskBase implements all object storage on plain files. The FS backend uses
// it directly; the SQLite backend embeds it and layers indexed metadata on top.
type diskBase struct {
	root string // <dataDir>/<bucket>
	mu   sync.Mutex
	cfg  *bucketConfig
}

// NewFSStore creates a backend that relies purely on OS file operations.
func NewFSStore(root, bucket string) (Store, error) {
	if err := os.MkdirAll(filepath.Join(root, objectsDir), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, uploadsDir), 0o755); err != nil {
		return nil, err
	}
	cfg, err := loadBucketConfig(root, bucket)
	if err != nil {
		return nil, err
	}
	return &diskBase{root: root, cfg: cfg}, nil
}

func (b *diskBase) Close() error { return nil }

func (b *diskBase) Exists() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, err := os.Stat(filepath.Join(b.root, bucketFile))
	return err == nil
}

func (b *diskBase) CreateBucket() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(b.root, objectsDir), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(b.root, uploadsDir), 0o755); err != nil {
		return err
	}
	cfg, err := loadBucketConfig(b.root, b.cfg.Name)
	if err != nil {
		return err
	}
	b.cfg = cfg
	return nil
}

func (b *diskBase) DeleteBucket() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	all, err := scanVersions(b.root)
	if err != nil {
		return err
	}
	if len(all) > 0 {
		return ErrBucketNotEmpty
	}
	udir := filepath.Join(b.root, uploadsDir)
	if ents, err := os.ReadDir(udir); err == nil && len(ents) > 0 {
		return ErrBucketNotEmpty
	}
	return os.RemoveAll(b.root)
}

func (b *diskBase) Created() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg.Created
}

func (b *diskBase) GetVersioning() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg.Versioning
}

func (b *diskBase) PutVersioning(status string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch status {
	case VersioningUnversioned, VersioningEnabled, VersioningSuspended:
	default:
		return fmt.Errorf("invalid versioning status %q", status)
	}
	b.cfg.Versioning = status
	return saveBucketConfig(b.root, b.cfg)
}

// versionsOf returns all versions of a key, newest first, IsLatest set on [0].
func (b *diskBase) versionsOf(key string) ([]Version, error) {
	vdir := filepath.Join(objectDir(b.root, key), versionsDir)
	ents, err := os.ReadDir(vdir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var vers []Version
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		var m versionMeta
		if err := readJSON(filepath.Join(vdir, e.Name(), metaFile), &m); err != nil {
			continue
		}
		vers = append(vers, Version{
			Key:          key,
			VersionID:    m.VersionID,
			Size:         m.Size,
			ETag:         m.ETag,
			ContentType:  m.ContentType,
			LastModified: m.LastModified,
			DeleteMarker: m.DeleteMarker,
		})
	}
	sort.SliceStable(vers, func(i, j int) bool {
		if vers[i].LastModified.Equal(vers[j].LastModified) {
			return vers[i].VersionID > vers[j].VersionID
		}
		return vers[i].LastModified.After(vers[j].LastModified)
	})
	if len(vers) > 0 {
		vers[0].IsLatest = true
	}
	return vers, nil
}

// resolveVersion finds a version by id; empty id means the latest version.
func (b *diskBase) resolveVersion(key, versionID string) (*Version, error) {
	vers, err := b.versionsOf(key)
	if err != nil {
		return nil, err
	}
	if len(vers) == 0 {
		return nil, &NotFoundError{Key: key}
	}
	if versionID == "" {
		v := vers[0]
		if v.DeleteMarker {
			return nil, &NotFoundError{Key: key}
		}
		return &v, nil
	}
	for i := range vers {
		if vers[i].VersionID == versionID {
			return &vers[i], nil
		}
	}
	return nil, &NotFoundError{Key: key, VersionID: versionID}
}

// writeVersionData streams r into the version dir and returns size+etag.
func writeVersionData(vdir string, r io.Reader) (int64, string, error) {
	if err := os.MkdirAll(vdir, 0o755); err != nil {
		return 0, "", err
	}
	tmp := filepath.Join(vdir, dataFile+".tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return 0, "", err
	}
	h := md5.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	cerr := f.Close()
	if err != nil {
		os.Remove(tmp)
		return 0, "", err
	}
	if cerr != nil {
		os.Remove(tmp)
		return 0, "", cerr
	}
	if err := os.Rename(tmp, filepath.Join(vdir, dataFile)); err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// putVersion creates a new version (or overwrites vid when forced) from r.
func (b *diskBase) putVersion(key string, r io.Reader, contentType, forceVID string) (*Version, error) {
	versioning := b.cfg.Versioning
	vid := forceVID
	if vid == "" {
		if versioning == VersioningEnabled {
			vid = newVersionID()
		} else {
			vid = NullVersionID
		}
	}
	vdir := versionDir(b.root, key, vid)
	n, etag, err := writeVersionData(vdir, r)
	if err != nil {
		return nil, err
	}
	if contentType == "" {
		contentType = "binary/octet-stream"
	}
	v := &Version{
		Key:          key,
		VersionID:    vid,
		Size:         n,
		ETag:         etag,
		ContentType:  contentType,
		LastModified: time.Now().UTC(),
		IsLatest:     true,
	}
	meta := versionMeta{
		VersionID:    v.VersionID,
		Size:         v.Size,
		ETag:         v.ETag,
		ContentType:  v.ContentType,
		LastModified: v.LastModified,
	}
	if err := writeJSONAtomic(filepath.Join(vdir, metaFile), &meta); err != nil {
		return nil, err
	}
	return v, nil
}

func (b *diskBase) PutObject(key string, body io.Reader, size int64, contentType string) (*Version, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	return b.putVersion(key, body, contentType, "")
}

func (b *diskBase) CopyObject(dstKey, srcKey, srcVersionID, contentType string) (*Version, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ValidateKey(dstKey); err != nil {
		return nil, err
	}
	src, err := b.resolveVersion(srcKey, srcVersionID)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(versionDir(b.root, srcKey, src.VersionID), dataFile))
	if err != nil {
		return nil, &NotFoundError{Key: srcKey, VersionID: srcVersionID}
	}
	defer f.Close()
	if contentType == "" {
		contentType = src.ContentType
	}
	return b.putVersion(dstKey, f, contentType, "")
}

func (b *diskBase) GetObject(key, versionID string) (*Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, err := b.resolveVersion(key, versionID)
	if err != nil {
		return nil, err
	}
	if v.DeleteMarker {
		return &Object{Version: *v}, nil
	}
	f, err := os.Open(filepath.Join(versionDir(b.root, key, v.VersionID), dataFile))
	if err != nil {
		return nil, &NotFoundError{Key: key, VersionID: v.VersionID}
	}
	return &Object{Version: *v, Body: f}, nil
}

func (b *diskBase) HeadObject(key, versionID string) (*Version, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.resolveVersion(key, versionID)
}

// removeVersionDir deletes one version and prunes the object dir if empty.
func (b *diskBase) removeVersionDir(key, versionID string) error {
	vdir := versionDir(b.root, key, versionID)
	if err := os.RemoveAll(vdir); err != nil {
		return err
	}
	objDir := objectDir(b.root, key)
	if ents, err := os.ReadDir(filepath.Join(objDir, versionsDir)); err == nil && len(ents) == 0 {
		os.Remove(filepath.Join(objDir, versionsDir))
		// prune empty parent dirs up to objects/
		dir := objDir
		for {
			parent := filepath.Dir(dir)
			if parent == filepath.Join(b.root, objectsDir) || parent == b.root {
				break
			}
			ents, err := os.ReadDir(parent)
			if err != nil || len(ents) > 0 {
				break
			}
			os.Remove(parent)
			dir = parent
		}
	}
	return nil
}

func (b *diskBase) DeleteObject(key, versionID string) (*Version, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	if versionID != "" {
		if _, err := b.resolveVersion(key, versionID); err != nil {
			var nf *NotFoundError
			if errors.As(err, &nf) {
				return nil, nil // deleting a missing version is a no-op (204)
			}
			return nil, err
		}
		if err := b.removeVersionDir(key, versionID); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if b.cfg.Versioning == VersioningEnabled {
		// create a delete marker as the new latest version
		vid := newVersionID()
		vdir := versionDir(b.root, key, vid)
		if err := os.MkdirAll(vdir, 0o755); err != nil {
			return nil, err
		}
		v := &Version{
			Key:          key,
			VersionID:    vid,
			LastModified: time.Now().UTC(),
			DeleteMarker: true,
			IsLatest:     true,
		}
		meta := versionMeta{
			VersionID:    vid,
			LastModified: v.LastModified,
			DeleteMarker: true,
		}
		if err := writeJSONAtomic(filepath.Join(vdir, metaFile), &meta); err != nil {
			return nil, err
		}
		return v, nil
	}
	// unversioned / suspended: delete the null version outright
	vers, err := b.versionsOf(key)
	if err != nil {
		return nil, err
	}
	if len(vers) == 0 {
		return nil, nil
	}
	for _, v := range vers {
		if v.VersionID == NullVersionID {
			if err := b.removeVersionDir(key, v.VersionID); err != nil {
				return nil, err
			}
		}
	}
	return nil, nil
}

func (b *diskBase) ListObjects(p ListParams, versions bool) (*ListResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	all, err := scanVersions(b.root)
	if err != nil {
		return nil, err
	}
	var flat []Version
	for _, vers := range all {
		if versions {
			flat = append(flat, vers...)
		} else {
			for _, v := range vers {
				if v.IsLatest && !v.DeleteMarker {
					flat = append(flat, v)
				}
				break
			}
		}
	}
	if versions {
		sortVersionsForListing(flat)
	} else {
		sort.SliceStable(flat, func(i, j int) bool { return flat[i].Key < flat[j].Key })
	}
	return applyListing(flat, versions, p), nil
}

// ---- multipart (disk) ----

func (b *diskBase) CreateMultipartUpload(key, contentType string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	uid := newVersionID()
	if contentType == "" {
		contentType = "binary/octet-stream"
	}
	um := uploadMeta{UploadID: uid, Key: key, Initiated: time.Now().UTC(), ContentType: contentType}
	if err := writeJSONAtomic(filepath.Join(uploadDir(b.root, uid), metaFile), &um); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(uploadDir(b.root, uid), partsDir), 0o755); err != nil {
		return "", err
	}
	return uid, nil
}

func (b *diskBase) uploadMetaOf(uid string) (*uploadMeta, error) {
	var um uploadMeta
	if err := readJSON(filepath.Join(uploadDir(b.root, uid), metaFile), &um); err != nil {
		if os.IsNotExist(err) {
			return nil, &UploadNotFoundError{UploadID: uid}
		}
		return nil, err
	}
	return &um, nil
}

func (b *diskBase) UploadPart(uploadID string, partNumber int, body io.Reader, size int64) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.uploadMetaOf(uploadID); err != nil {
		return "", err
	}
	if partNumber < 1 || partNumber > 10000 {
		return "", fmt.Errorf("invalid part number %d", partNumber)
	}
	ppath := partPath(b.root, uploadID, partNumber)
	if err := os.MkdirAll(filepath.Dir(ppath), 0o755); err != nil {
		return "", err
	}
	f, err := os.Create(ppath + ".tmp")
	if err != nil {
		return "", err
	}
	h := md5.New()
	n, err := io.Copy(io.MultiWriter(f, h), body)
	cerr := f.Close()
	if err != nil || cerr != nil {
		os.Remove(ppath + ".tmp")
		if err == nil {
			err = cerr
		}
		return "", err
	}
	if err := os.Rename(ppath+".tmp", ppath); err != nil {
		return "", err
	}
	etag := hex.EncodeToString(h.Sum(nil))
	pm := partMeta{PartNumber: partNumber, ETag: etag, Size: n}
	if err := writeJSONAtomic(partMetaPath(b.root, uploadID, partNumber), &pm); err != nil {
		return "", err
	}
	return etag, nil
}

const minPartSize = 5 << 20 // 5 MiB

func (b *diskBase) CompleteMultipartUpload(uploadID string, parts []Part) (*Version, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	um, err := b.uploadMetaOf(uploadID)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("no parts provided")
	}
	sorted := make([]Part, len(parts))
	copy(sorted, parts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].PartNumber < sorted[j].PartNumber })
	// validate
	h := md5.New()
	var total int64
	prev := 0
	for i, p := range sorted {
		if p.PartNumber <= prev {
			return nil, fmt.Errorf("parts must be in ascending order")
		}
		prev = p.PartNumber
		var pm partMeta
		if err := readJSON(partMetaPath(b.root, uploadID, p.PartNumber), &pm); err != nil {
			return nil, ErrInvalidPart
		}
		if pm.ETag != strings.Trim(p.ETag, "\"") {
			return nil, ErrInvalidPart
		}
		if i < len(sorted)-1 && pm.Size < minPartSize {
			return nil, ErrPartTooSmall
		}
		raw, err := hex.DecodeString(pm.ETag)
		if err != nil {
			return nil, ErrInvalidPart
		}
		h.Write(raw)
		total += pm.Size
	}
	etag := hex.EncodeToString(h.Sum(nil)) + "-" + strconv.Itoa(len(sorted))
	// assemble the object
	vid := NullVersionID
	if b.cfg.Versioning == VersioningEnabled {
		vid = newVersionID()
	}
	vdir := versionDir(b.root, um.Key, vid)
	if err := os.MkdirAll(vdir, 0o755); err != nil {
		return nil, err
	}
	out, err := os.Create(filepath.Join(vdir, dataFile+".tmp"))
	if err != nil {
		return nil, err
	}
	for _, p := range sorted {
		pf, err := os.Open(partPath(b.root, uploadID, p.PartNumber))
		if err != nil {
			out.Close()
			os.Remove(filepath.Join(vdir, dataFile+".tmp"))
			return nil, ErrInvalidPart
		}
		_, cpErr := io.Copy(out, pf)
		pf.Close()
		if cpErr != nil {
			out.Close()
			os.Remove(filepath.Join(vdir, dataFile+".tmp"))
			return nil, cpErr
		}
	}
	if err := out.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(filepath.Join(vdir, dataFile+".tmp"), filepath.Join(vdir, dataFile)); err != nil {
		return nil, err
	}
	v := &Version{
		Key:          um.Key,
		VersionID:    vid,
		Size:         total,
		ETag:         etag,
		ContentType:  um.ContentType,
		LastModified: time.Now().UTC(),
		IsLatest:     true,
	}
	meta := versionMeta{
		VersionID:    v.VersionID,
		Size:         v.Size,
		ETag:         v.ETag,
		ContentType:  v.ContentType,
		LastModified: v.LastModified,
	}
	if err := writeJSONAtomic(filepath.Join(vdir, metaFile), &meta); err != nil {
		return nil, err
	}
	os.RemoveAll(uploadDir(b.root, uploadID))
	return v, nil
}

func (b *diskBase) AbortMultipartUpload(uploadID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.uploadMetaOf(uploadID); err != nil {
		return err
	}
	return os.RemoveAll(uploadDir(b.root, uploadID))
}

func (b *diskBase) ListMultipartUploads(p ListUploadsParams) (*ListUploadsResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	udir := filepath.Join(b.root, uploadsDir)
	ents, err := os.ReadDir(udir)
	if os.IsNotExist(err) {
		return &ListUploadsResult{}, nil
	}
	if err != nil {
		return nil, err
	}
	var uploads []UploadInfo
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		var um uploadMeta
		if err := readJSON(filepath.Join(udir, e.Name(), metaFile), &um); err != nil {
			continue
		}
		uploads = append(uploads, UploadInfo{Key: um.Key, UploadID: um.UploadID, Initiated: um.Initiated})
	}
	sort.SliceStable(uploads, func(i, j int) bool {
		if uploads[i].Key != uploads[j].Key {
			return uploads[i].Key < uploads[j].Key
		}
		return uploads[i].Initiated.Before(uploads[j].Initiated)
	})
	// resume
	if km := untoken(p.KeyMarker); km != "" {
		start := len(uploads)
		for i, u := range uploads {
			if u.Key > km || (u.Key == km && u.UploadID > p.UploadIDMarker) {
				start = i
				break
			}
		}
		uploads = uploads[start:]
	}
	max := p.MaxUploads
	if max <= 0 || max > 1000 {
		max = 1000
	}
	res := &ListUploadsResult{}
	prefixes := map[string]bool{}
	count := 0
	for _, u := range uploads {
		if p.Prefix != "" && !hasPrefix(u.Key, p.Prefix) {
			continue
		}
		if p.Delimiter != "" {
			rest := u.Key[len(p.Prefix):]
			if idx := indexOf(rest, p.Delimiter); idx >= 0 {
				cp := p.Prefix + rest[:idx+len(p.Delimiter)]
				if !prefixes[cp] {
					prefixes[cp] = true
					res.CommonPrefixes = append(res.CommonPrefixes, cp)
				}
				continue
			}
		}
		res.Uploads = append(res.Uploads, u)
		count++
		if count >= max {
			res.IsTruncated = true
			last := res.Uploads[len(res.Uploads)-1]
			res.NextKeyMarker = tokenFor(last.Key)
			res.NextUploadIDMarker = last.UploadID
			break
		}
	}
	sort.Strings(res.CommonPrefixes)
	return res, nil
}

func (b *diskBase) ListParts(uploadID string, maxParts int, partNumberMarker int) (*ListPartsResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.uploadMetaOf(uploadID); err != nil {
		return nil, err
	}
	pdir := filepath.Join(uploadDir(b.root, uploadID), partsDir)
	ents, err := os.ReadDir(pdir)
	if os.IsNotExist(err) {
		return &ListPartsResult{}, nil
	}
	if err != nil {
		return nil, err
	}
	var parts []PartInfo
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), "."+metaFile) {
			continue
		}
		var pm partMeta
		if err := readJSON(filepath.Join(pdir, e.Name()), &pm); err != nil {
			continue
		}
		info, ierr := e.Info()
		modTime := time.Now().UTC()
		if ierr == nil {
			modTime = info.ModTime().UTC()
		}
		parts = append(parts, PartInfo{
			PartNumber:   pm.PartNumber,
			Size:         pm.Size,
			ETag:         pm.ETag,
			LastModified: modTime,
		})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })
	if maxParts <= 0 || maxParts > 1000 {
		maxParts = 1000
	}
	res := &ListPartsResult{}
	count := 0
	for _, p := range parts {
		if p.PartNumber <= partNumberMarker {
			continue
		}
		res.Parts = append(res.Parts, p)
		count++
		if count >= maxParts {
			res.IsTruncated = true
			res.NextMarker = res.Parts[len(res.Parts)-1].PartNumber
			break
		}
	}
	return res, nil
}
