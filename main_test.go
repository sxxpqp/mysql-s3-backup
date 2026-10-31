package main

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

func TestScheduledBackupContinuesAfterFailureAndStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls := 0
	runScheduled(ctx, time.Millisecond, func(context.Context) error {
		calls++
		if calls == 2 {
			cancel()
		}
		return errors.New("upload unavailable")
	})
	if calls != 2 {
		t.Fatalf("expected retry after failure, got %d calls", calls)
	}
	// A cancelled scheduler must not start another backup.
	runScheduled(ctx, time.Millisecond, func(context.Context) error { t.Fatal("backup started after cancellation"); return nil })
}

func TestCronSchedule(t *testing.T) {
	schedule, loc, err := parseCron("0 2 * * *", "Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Date(2026, 10, 31, 1, 59, 0, 0, loc)
	want := time.Date(2026, 10, 31, 2, 0, 0, 0, loc)
	if got := schedule.Next(before); !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
	if got := schedule.Next(want); !got.Equal(want.AddDate(0, 0, 1)) {
		t.Fatalf("must wait for next day at exact tick: %s", got)
	}
	for _, value := range []string{"invalid", "60 2 * * *", "0 0 0 * * *", "0 0 31 2 *"} {
		if _, _, err := parseCron(value, "Asia/Shanghai"); err == nil {
			t.Errorf("accepted invalid schedule %q", value)
		}
	}
	if _, _, err := parseCron("0 2 * * *", "invalid/zone"); err == nil {
		t.Fatal("accepted invalid timezone")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runCron(ctx, schedule, loc, func(context.Context) error { t.Fatal("ran after cancel"); return nil })
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
