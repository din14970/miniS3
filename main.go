// miniS3 serves the S3 API on top of a local folder, with a single bucket,
// optional SQLite metadata storage, and a small web UI.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/din14970/miniS3/internal/api"
	"github.com/din14970/miniS3/internal/sigv4"
	"github.com/din14970/miniS3/internal/store"
	"github.com/din14970/miniS3/internal/web"
)

func main() {
	var (
		addr      = flag.String("addr", ":9000", "listen address for the S3 API and web UI")
		dataDir   = flag.String("data-dir", "./data", "local folder backing the bucket")
		bucket    = flag.String("bucket", "bucket", "name of the single served bucket")
		dbMode    = flag.String("db", "auto", "metadata storage: auto (sqlite file in data dir), memory (sqlite in-memory), file (sqlite at --db-path), none (no DB, OS files only)")
		dbPath    = flag.String("db-path", "", "sqlite file path when --db=file (default: <data-dir>/.minis3.db)")
		accessKey = flag.String("access-key", envOr("MINIS3_ACCESS_KEY", ""), "access key; if set (with --secret-key) requests must carry a valid SigV4 signature")
		secretKey = flag.String("secret-key", envOr("MINIS3_SECRET_KEY", ""), "secret key for SigV4 verification")
		region    = flag.String("region", "us-east-1", "region reported via GetBucketLocation")
		noUI      = flag.Bool("no-ui", false, "disable the web UI at /ui")
	)
	flag.Parse()

	if err := run(*addr, *dataDir, *bucket, *dbMode, *dbPath, *accessKey, *secretKey, *region, *noUI); err != nil {
		log.Fatal(err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func run(addr, dataDir, bucket, dbMode, dbPath, accessKey, secretKey, region string, noUI bool) error {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(abs, bucket), 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	root := filepath.Join(abs, bucket)

	var st store.Store
	var dbDesc string
	switch dbMode {
	case "auto", "file":
		p := dbPath
		if p == "" {
			p = filepath.Join(abs, ".minis3.db")
		}
		st, err = store.NewSQLiteStore(root, bucket, p)
		if err != nil {
			return err
		}
		dbDesc = "sqlite file " + p
	case "memory":
		st, err = store.NewSQLiteStore(root, bucket, ":memory:")
		if err != nil {
			return err
		}
		dbDesc = "sqlite in-memory"
	case "none":
		st, err = store.NewFSStore(root, bucket)
		if err != nil {
			return err
		}
		dbDesc = "none (OS file operations only)"
	default:
		return fmt.Errorf("unknown --db mode %q (want auto|memory|file|none)", dbMode)
	}
	defer st.Close()

	auth := &sigv4.Auth{AccessKey: accessKey, SecretKey: secretKey}
	srv := &api.Server{
		Store:  st,
		Bucket: bucket,
		Region: region,
		Auth:   auth,
	}
	if !noUI {
		ui, err := web.New(st, bucket)
		if err != nil {
			return err
		}
		srv.UI = ui
	}

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		log.Println("shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(ctx)
	}()

	authDesc := "permissive (any credentials accepted)"
	if auth.Enabled() {
		authDesc = "SigV4 verified (access key " + accessKey + ")"
	}
	log.Printf("miniS3 listening on %s", addr)
	log.Printf("  bucket:    %s (data dir: %s)", bucket, abs)
	log.Printf("  metadata:  %s", dbDesc)
	log.Printf("  auth:      %s", authDesc)
	if !noUI {
		log.Printf("  web UI:    http://localhost%s/ui/", displayAddr(addr))
	}
	log.Printf("  S3 API:    path-style, e.g. http://localhost%s/%s/", displayAddr(addr), bucket)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func displayAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	return addr
}
