package store

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS objects (
  key TEXT NOT NULL,
  version_id TEXT NOT NULL,
  size INTEGER NOT NULL,
  etag TEXT NOT NULL,
  content_type TEXT NOT NULL,
  last_modified TEXT NOT NULL,
  last_modified_ns INTEGER NOT NULL,
  delete_marker INTEGER NOT NULL DEFAULT 0,
  is_latest INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (key, version_id)
);
CREATE INDEX IF NOT EXISTS idx_objects_latest ON objects(is_latest);
CREATE TABLE IF NOT EXISTS uploads (
  upload_id TEXT PRIMARY KEY,
  key TEXT NOT NULL,
  initiated TEXT NOT NULL,
  initiated_ns INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS parts (
  upload_id TEXT NOT NULL,
  part_number INTEGER NOT NULL,
  etag TEXT NOT NULL,
  size INTEGER NOT NULL,
  PRIMARY KEY (upload_id, part_number)
);
`

// sqliteStore keeps object metadata in SQLite while object bytes (and the
// meta.json sidecars used for auto-population) live on disk via diskBase.
type sqliteStore struct {
	*diskBase
	db *sql.DB
	mu sync.Mutex
}

// NewSQLiteStore opens (creating if needed) a SQLite metadata DB at dsn
// (":memory:" or a file path) for the bucket rooted at root. If the DB has no
// object rows, it is populated automatically by scanning the data directory.
func NewSQLiteStore(root, bucket, dsn string) (Store, error) {
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
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=10000`); err != nil {
		return nil, err
	}
	if dsn != ":memory:" {
		if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
			return nil, err
		}
		if _, err := db.Exec(`PRAGMA synchronous=NORMAL`); err != nil {
			return nil, err
		}
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("create schema: %w", err)
	}
	s := &sqliteStore{diskBase: &diskBase{root: root, cfg: cfg}, db: db}
	if err := s.populate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("auto-populate: %w", err)
	}
	return s, nil
}

// populate scans the data directory and fills the DB when it is empty.
func (s *sqliteStore) populate() error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM objects`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		all, err := scanVersions(s.root)
		if err != nil {
			return err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		stmt, err := tx.Prepare(`INSERT OR REPLACE INTO objects
		  (key, version_id, size, etag, content_type, last_modified, last_modified_ns, delete_marker, is_latest)
		  VALUES (?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			tx.Rollback()
			return err
		}
		for _, vers := range all {
			for _, v := range vers {
				isLatest := 0
				if v.IsLatest {
					isLatest = 1
				}
				dm := 0
				if v.DeleteMarker {
					dm = 1
				}
				if _, err := stmt.Exec(v.Key, v.VersionID, v.Size, v.ETag, v.ContentType,
					v.LastModified.Format(time.RFC3339Nano), v.LastModified.UnixNano(), dm, isLatest); err != nil {
					stmt.Close()
					tx.Rollback()
					return err
				}
			}
		}
		stmt.Close()
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	var un int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM uploads`).Scan(&un); err != nil {
		return err
	}
	if un == 0 {
		udir := filepath.Join(s.root, uploadsDir)
		ents, err := os.ReadDir(udir)
		if err == nil {
			for _, e := range ents {
				if !e.IsDir() {
					continue
				}
				var um uploadMeta
				if err := readJSON(filepath.Join(udir, e.Name(), metaFile), &um); err != nil {
					continue
				}
				if _, err := s.db.Exec(`INSERT OR REPLACE INTO uploads (upload_id, key, initiated, initiated_ns) VALUES (?,?,?,?)`,
					um.UploadID, um.Key, um.Initiated.Format(time.RFC3339Nano), um.Initiated.UnixNano()); err != nil {
					return err
				}
				pdir := filepath.Join(udir, e.Name(), partsDir)
				pents, err := os.ReadDir(pdir)
				if err != nil {
					continue
				}
				for _, pe := range pents {
					var pm partMeta
					if err := readJSON(filepath.Join(pdir, pe.Name()), &pm); err != nil {
						continue
					}
					if _, err := s.db.Exec(`INSERT OR REPLACE INTO parts (upload_id, part_number, etag, size) VALUES (?,?,?,?)`,
						um.UploadID, pm.PartNumber, pm.ETag, pm.Size); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (s *sqliteStore) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

func (s *sqliteStore) DeleteBucket() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.diskBase.DeleteBucket(); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM objects`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM uploads`); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM parts`)
	return err
}

func rowToVersion(rows interface{ Scan(...any) error }) (Version, error) {
	var v Version
	var lm string
	var ns int64
	var dm, isLatest int
	err := rows.Scan(&v.Key, &v.VersionID, &v.Size, &v.ETag, &v.ContentType, &lm, &ns, &dm, &isLatest)
	if err != nil {
		return v, err
	}
	v.LastModified = time.Unix(0, ns).UTC()
	v.DeleteMarker = dm == 1
	v.IsLatest = isLatest == 1
	return v, nil
}

func (s *sqliteStore) ListObjects(p ListParams, versions bool) (*ListResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var query string
	if versions {
		query = `SELECT key, version_id, size, etag, content_type, last_modified, last_modified_ns, delete_marker, is_latest
		         FROM objects ORDER BY key ASC, last_modified_ns DESC, version_id DESC`
	} else {
		query = `SELECT key, version_id, size, etag, content_type, last_modified, last_modified_ns, delete_marker, is_latest
		         FROM objects WHERE is_latest=1 AND delete_marker=0 ORDER BY key ASC`
	}
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	var all []Version
	for rows.Next() {
		v, err := rowToVersion(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return applyListing(all, versions, p), nil
}

func (s *sqliteStore) HeadObject(key, versionID string) (*Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.head(key, versionID)
	if err != nil {
		return nil, err
	}
	return v, nil
}

func (s *sqliteStore) head(key, versionID string) (*Version, error) {
	var row *sql.Row
	if versionID == "" {
		row = s.db.QueryRow(`SELECT key, version_id, size, etag, content_type, last_modified, last_modified_ns, delete_marker, is_latest
		                     FROM objects WHERE key=? AND is_latest=1`, key)
	} else {
		row = s.db.QueryRow(`SELECT key, version_id, size, etag, content_type, last_modified, last_modified_ns, delete_marker, is_latest
		                     FROM objects WHERE key=? AND version_id=?`, key, versionID)
	}
	v, err := rowToVersion(row)
	if err == sql.ErrNoRows {
		return nil, &NotFoundError{Key: key, VersionID: versionID}
	}
	if err != nil {
		return nil, err
	}
	if versionID == "" && v.DeleteMarker {
		return nil, &NotFoundError{Key: key}
	}
	return &v, nil
}

func (s *sqliteStore) GetObject(key, versionID string) (*Object, error) {
	s.mu.Lock()
	v, err := s.head(key, versionID)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if v.DeleteMarker {
		return &Object{Version: *v}, nil
	}
	f, err := os.Open(filepath.Join(versionDir(s.root, key, v.VersionID), dataFile))
	if err != nil {
		return nil, &NotFoundError{Key: key, VersionID: v.VersionID}
	}
	return &Object{Version: *v, Body: f}, nil
}

func (s *sqliteStore) insertVersion(tx *sql.Tx, v *Version) error {
	if _, err := tx.Exec(`UPDATE objects SET is_latest=0 WHERE key=?`, v.Key); err != nil {
		return err
	}
	dm := 0
	if v.DeleteMarker {
		dm = 1
	}
	_, err := tx.Exec(`INSERT OR REPLACE INTO objects
	  (key, version_id, size, etag, content_type, last_modified, last_modified_ns, delete_marker, is_latest)
	  VALUES (?,?,?,?,?,?,?,?,1)`,
		v.Key, v.VersionID, v.Size, v.ETag, v.ContentType,
		v.LastModified.Format(time.RFC3339Nano), v.LastModified.UnixNano(), dm)
	return err
}

func (s *sqliteStore) PutObject(key string, body io.Reader, size int64, contentType string) (*Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.diskBase.PutObject(key, body, size, contentType)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	if err := s.insertVersion(tx, v); err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *sqliteStore) CopyObject(dstKey, srcKey, srcVersionID, contentType string) (*Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.diskBase.CopyObject(dstKey, srcKey, srcVersionID, contentType)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	if err := s.insertVersion(tx, v); err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *sqliteStore) DeleteObject(key, versionID string) (*Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.diskBase.DeleteObject(key, versionID)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	if versionID != "" {
		if _, err := tx.Exec(`DELETE FROM objects WHERE key=? AND version_id=?`, key, versionID); err != nil {
			tx.Rollback()
			return nil, err
		}
		// re-mark latest among remaining versions
		if _, err := tx.Exec(`UPDATE objects SET is_latest=0 WHERE key=?`, key); err != nil {
			tx.Rollback()
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE objects SET is_latest=1 WHERE key=? AND version_id=(
		  SELECT version_id FROM objects WHERE key=? ORDER BY last_modified_ns DESC, version_id DESC LIMIT 1)`, key, key); err != nil {
			tx.Rollback()
			return nil, err
		}
	} else if v != nil {
		if err := s.insertVersion(tx, v); err != nil {
			tx.Rollback()
			return nil, err
		}
	} else {
		if _, err := tx.Exec(`DELETE FROM objects WHERE key=? AND version_id=?`, key, NullVersionID); err != nil {
			tx.Rollback()
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *sqliteStore) CreateMultipartUpload(key, contentType string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	uid, err := s.diskBase.CreateMultipartUpload(key, contentType)
	if err != nil {
		return "", err
	}
	um, err := s.diskBase.uploadMetaOf(uid)
	if err != nil {
		return "", err
	}
	if _, err := s.db.Exec(`INSERT OR REPLACE INTO uploads (upload_id, key, initiated, initiated_ns) VALUES (?,?,?,?)`,
		uid, um.Key, um.Initiated.Format(time.RFC3339Nano), um.Initiated.UnixNano()); err != nil {
		return "", err
	}
	return uid, nil
}

func (s *sqliteStore) UploadPart(uploadID string, partNumber int, body io.Reader, size int64) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	etag, err := s.diskBase.UploadPart(uploadID, partNumber, body, size)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(partPath(s.root, uploadID, partNumber))
	if err != nil {
		return "", err
	}
	if _, err := s.db.Exec(`INSERT OR REPLACE INTO parts (upload_id, part_number, etag, size) VALUES (?,?,?,?)`,
		uploadID, partNumber, etag, fi.Size()); err != nil {
		return "", err
	}
	return etag, nil
}

func (s *sqliteStore) CompleteMultipartUpload(uploadID string, parts []Part) (*Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.diskBase.CompleteMultipartUpload(uploadID, parts)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	if err := s.insertVersion(tx, v); err != nil {
		tx.Rollback()
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM parts WHERE upload_id=?`, uploadID); err != nil {
		tx.Rollback()
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM uploads WHERE upload_id=?`, uploadID); err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *sqliteStore) AbortMultipartUpload(uploadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.diskBase.AbortMultipartUpload(uploadID); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM parts WHERE upload_id=?`, uploadID); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM uploads WHERE upload_id=?`, uploadID)
	return err
}

func (s *sqliteStore) ListMultipartUploads(p ListUploadsParams) (*ListUploadsResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT key, upload_id, initiated, initiated_ns FROM uploads ORDER BY key ASC, initiated_ns ASC, upload_id ASC`)
	if err != nil {
		return nil, err
	}
	var uploads []UploadInfo
	for rows.Next() {
		var key, uid, init string
		var ns int64
		if err := rows.Scan(&key, &uid, &init, &ns); err != nil {
			rows.Close()
			return nil, err
		}
		uploads = append(uploads, UploadInfo{Key: key, UploadID: uid, Initiated: time.Unix(0, ns).UTC()})
	}
	rows.Close()
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

func (s *sqliteStore) ListParts(uploadID string, maxParts int, partNumberMarker int) (*ListPartsResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM uploads WHERE upload_id=?`, uploadID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, &UploadNotFoundError{UploadID: uploadID}
	}
	rows, err := s.db.Query(`SELECT part_number, etag, size FROM parts WHERE upload_id=? ORDER BY part_number ASC`, uploadID)
	if err != nil {
		return nil, err
	}
	var parts []PartInfo
	for rows.Next() {
		var p PartInfo
		if err := rows.Scan(&p.PartNumber, &p.ETag, &p.Size); err != nil {
			rows.Close()
			return nil, err
		}
		parts = append(parts, p)
	}
	rows.Close()
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
