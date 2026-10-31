package main

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("FAKE_DUMP"); mode != "" {
		if mode == "fail" {
			os.Exit(2)
		}
		_, _ = os.Stdout.WriteString("CREATE DATABASE example;\n")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestBackup(t *testing.T) {
	for _, mode := range []string{"ok", "fail"} {
		t.Run(mode, func(t *testing.T) {
			var uploads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "PUT" || !strings.HasPrefix(r.URL.Path, "/backups/mysql/") {
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					w.WriteHeader(400)
					return
				}
				var body io.Reader = r.Body
				if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") || strings.Contains(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
					body = httputil.NewChunkedReader(body)
				}
				reader, err := gzip.NewReader(body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				defer reader.Close()
				data, err := io.ReadAll(reader)
				if err != nil || string(data) != "CREATE DATABASE example;\n" {
					t.Errorf("bad dump: %q %v", data, err)
				}
				uploads.Add(1)
				w.Header().Set("ETag", `"test"`)
				w.WriteHeader(200)
			}))
			defer server.Close()
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range map[string]string{"MYSQL_HOST": "localhost", "MYSQL_USER": "backup", "S3_ENDPOINT": server.URL, "S3_BUCKET": "backups", "AWS_ACCESS_KEY_ID": "test", "AWS_SECRET_ACCESS_KEY": "testsecret", "MYSQLDUMP_BIN": exe, "FAKE_DUMP": mode, "MYSQL_DATABASES": "app", "S3_PREFIX": "mysql"} {
				t.Setenv(k, v)
			}
			err = run(context.Background())
			if mode == "ok" {
				if err != nil {
					t.Fatal(err)
				}
				if uploads.Load() != 1 {
					t.Fatal("expected one upload")
				}
			} else {
				if err == nil {
					t.Fatal("expected dump error")
				}
				if uploads.Load() != 0 {
					t.Fatal("failed dump was uploaded")
				}
			}
		})
	}
}
