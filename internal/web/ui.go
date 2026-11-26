// Package web implements the minimal browser UI served under /ui.
package web

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/din14970/miniS3/internal/store"
)

//go:embed templates static
var assets embed.FS

// UI is the web UI handler.
type UI struct {
	Store  store.Store
	Bucket string
	tmpl   *template.Template
	static http.Handler
}

// New creates the UI handler.
func New(st store.Store, bucket string) (*UI, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"humanSize": humanSize,
		"queryEscape": func(s string) string {
			return url.QueryEscape(s)
		},
		"browseURL": func(prefix string) string {
			return "/ui/browse?delimiter=/&prefix=" + url.QueryEscape(prefix)
		},
		"fmtTime": func(t time.Time) string {
			return t.UTC().Format("2006-01-02 15:04:05 UTC")
		},
	}).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	fsys, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	return &UI{
		Store:  st,
		Bucket: bucket,
		tmpl:   tmpl,
		static: http.StripPrefix("/ui/static/", http.FileServer(http.FS(fsys))),
	}, nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func (u *UI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/ui" || r.URL.Path == "/ui/":
		u.index(w, r)
	case r.URL.Path == "/ui/browse":
		u.browse(w, r)
	case r.URL.Path == "/ui/upload" && r.Method == http.MethodPost:
		u.upload(w, r)
	case r.URL.Path == "/ui/download":
		u.download(w, r)
	case r.URL.Path == "/ui/delete" && r.Method == http.MethodPost:
		u.delete(w, r)
	case r.URL.Path == "/ui/versions":
		u.versions(w, r)
	case r.URL.Path == "/ui/versioning" && r.Method == http.MethodGet:
		u.versioningForm(w, r)
	case r.URL.Path == "/ui/versioning" && r.Method == http.MethodPost:
		u.versioningSet(w, r)
	case strings.HasPrefix(r.URL.Path, "/ui/static/"):
		u.static.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (u *UI) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := u.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type indexData struct {
	Bucket    string
	Prefix    string
	BrowseURL string
}

func (u *UI) index(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	u.render(w, "index.html", indexData{
		Bucket:    u.Bucket,
		Prefix:    prefix,
		BrowseURL: "/ui/browse?delimiter=/&prefix=" + url.QueryEscape(prefix),
	})
}

type crumb struct {
	Name string
	URL  string
}

type objectRow struct {
	Key       string
	KeyEsc    string
	Size      int64
	SizeHuman string
	Modified  string
	ETag      string
	VersionID string
}

type prefixRow struct {
	Name string
	URL  string
}

type listingData struct {
	Prefix   string
	Crumbs   []crumb
	Prefixes []prefixRow
	Objects  []objectRow
}

func (u *UI) listingData(prefix string) listingData {
	d := listingData{Prefix: prefix}
	if prefix != "" {
		segs := strings.Split(strings.TrimSuffix(prefix, "/"), "/")
		acc := ""
		for _, s := range segs {
			if s == "" {
				continue
			}
			acc += s + "/"
			d.Crumbs = append(d.Crumbs, crumb{Name: s + "/", URL: "/ui/browse?delimiter=/&prefix=" + url.QueryEscape(acc)})
		}
	}
	res, err := u.Store.ListObjects(store.ListParams{Prefix: prefix, Delimiter: "/", MaxKeys: 500}, false)
	if err == nil {
		for _, cp := range res.CommonPrefixes {
			name := strings.TrimPrefix(cp, prefix)
			d.Prefixes = append(d.Prefixes, prefixRow{
				Name: name,
				URL:  "/ui/browse?delimiter=/&prefix=" + url.QueryEscape(cp),
			})
		}
		for _, v := range res.Objects {
			d.Objects = append(d.Objects, objectRow{
				Key:       v.Key,
				KeyEsc:    url.QueryEscape(v.Key),
				Size:      v.Size,
				SizeHuman: humanSize(v.Size),
				Modified:  v.LastModified.UTC().Format("2006-01-02 15:04:05"),
				ETag:      v.ETag,
				VersionID: v.VersionID,
			})
		}
	}
	return d
}

func (u *UI) browse(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	u.render(w, "listing.html", u.listingData(prefix))
}

func (u *UI) upload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 30); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	prefix := r.FormValue("prefix")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file", http.StatusBadRequest)
		return
	}
	defer file.Close()
	key := prefix + path.Base(header.Filename)
	if err := store.ValidateKey(key); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ct := header.Header.Get("Content-Type")
	if _, err := u.Store.PutObject(key, file, header.Size, ct); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, "/ui/?prefix="+url.QueryEscape(prefix), http.StatusSeeOther)
		return
	}
	u.render(w, "listing.html", u.listingData(prefix))
}

func (u *UI) download(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	versionID := r.URL.Query().Get("versionId")
	obj, err := u.Store.GetObject(key, versionID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer obj.Close()
	w.Header().Set("Content-Type", obj.ContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", obj.Size))
	w.Header().Set("Content-Disposition", `attachment; filename="`+path.Base(key)+`"`)
	w.Header().Set("ETag", `"`+obj.ETag+`"`)
	w.WriteHeader(http.StatusOK)
	io.Copy(w, obj.Body)
}

func (u *UI) delete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key := r.FormValue("key")
	versionID := r.FormValue("versionId")
	prefix := r.FormValue("prefix")
	panel := r.FormValue("panel")
	if _, err := u.Store.DeleteObject(key, versionID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, "/ui/?prefix="+url.QueryEscape(prefix), http.StatusSeeOther)
		return
	}
	if panel == "versions" {
		u.render(w, "versions.html", u.versionsData(key))
		return
	}
	u.render(w, "listing.html", u.listingData(prefix))
}

type versionRow struct {
	VersionID    string
	Size         int64
	SizeHuman    string
	Modified     string
	ETag         string
	IsLatest     bool
	DeleteMarker bool
}

type versionsData struct {
	Key     string
	KeyEsc  string
	Prefix  string
	Versions []versionRow
}

func (u *UI) versionsData(key string) versionsData {
	d := versionsData{Key: key, KeyEsc: url.QueryEscape(key)}
	res, err := u.Store.ListObjects(store.ListParams{Prefix: key, MaxKeys: 1000}, true)
	if err == nil {
		for _, v := range res.Objects {
			if v.Key != key {
				continue
			}
			d.Versions = append(d.Versions, versionRow{
				VersionID:    v.VersionID,
				Size:         v.Size,
				SizeHuman:    humanSize(v.Size),
				Modified:     v.LastModified.UTC().Format("2006-01-02 15:04:05"),
				ETag:         v.ETag,
				IsLatest:     v.IsLatest,
				DeleteMarker: v.DeleteMarker,
			})
		}
	}
	return d
}

func (u *UI) versions(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	u.render(w, "versions.html", u.versionsData(key))
}

func (u *UI) versioningForm(w http.ResponseWriter, r *http.Request) {
	u.render(w, "versioning.html", map[string]string{"Status": u.Store.GetVersioning()})
}

func (u *UI) versioningSet(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	status := r.FormValue("status")
	if err := u.Store.PutVersioning(status); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.render(w, "versioning.html", map[string]string{"Status": u.Store.GetVersioning()})
}
