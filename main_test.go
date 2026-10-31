package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBackupObjectKey(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	instant := time.Date(2026, 10, 31, 16, 10, 17, 123456789, time.UTC).In(location)
	want := "mysql/20261101/app-prod-20261101T001017.123456789+0800.sql.gz"
	if got := backupObjectKey("mysql", "app-prod", instant); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := backupObjectKey("mysql", "a/b", instant); !strings.Contains(got, "/a%2Fb-") {
		t.Fatalf("database created extra path segment: %q", got)
	}
}

func TestLogTimezone(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	w := timezoneLogWriter{output: &output, location: location}
	message := []byte("backup completed\n")
	n, err := w.Write(message)
	if err != nil || n != len(message) {
		t.Fatalf("write: %d %v", n, err)
	}
	parts := strings.SplitN(output.String(), " ", 2)
	stamp, err := time.Parse(time.RFC3339, parts[0])
	if err != nil {
		t.Fatal(err)
	}
	_, offset := stamp.Zone()
	if offset != 8*60*60 || parts[1] != string(message) {
		t.Fatalf("unexpected log: %s", output.String())
	}
}

func TestMain(m *testing.M) {
	if mode := os.Getenv("FAKE_DUMP"); mode != "" {
		for _, arg := range os.Args[1:] {
			if arg == "--execute=SELECT 1" {
				if mode == "mysql-fail" {
					os.Exit(2)
				}
				if mode == "mysql-bad-response" {
					fmt.Print("unexpected\n")
					os.Exit(0)
				}
				fmt.Print("1\n")
				os.Exit(0)
			}
			if strings.HasPrefix(arg, "--execute=") {
				fmt.Print("61707031\n61707032\n6d7973716c\n")
				os.Exit(0)
			}
		}
		database := os.Args[len(os.Args)-1]
		if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--databases" {
			os.Exit(3)
		}
		if mode == "fail" || (mode == "partial" && database == "app2") {
			os.Exit(2)
		}
		fmt.Printf("CREATE DATABASE %s;\n", database)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestStartupChecks(t *testing.T) {
	for _, mode := range []string{"ok", "mysql-fail", "mysql-bad-response", "s3-denied", "delete-denied", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			var puts, deletes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, "/backups/mysql/.connection-checks/") {
					t.Errorf("unexpected probe path %s", r.URL.Path)
				}
				_, _ = io.Copy(io.Discard, r.Body)
				switch r.Method {
				case "PUT":
					puts.Add(1)
					if mode == "s3-denied" {
						w.WriteHeader(403)
						_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code><Message>denied</Message></Error>")
						return
					}
					w.Header().Set("ETag", `"probe"`)
					w.WriteHeader(200)
				case "DELETE":
					deletes.Add(1)
					if mode == "delete-denied" {
						w.WriteHeader(403)
						_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code><Message>denied</Message></Error>")
						return
					}
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected method %s", r.Method)
					w.WriteHeader(400)
				}
			}))
			defer server.Close()
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range map[string]string{"MYSQL_HOST": "localhost", "MYSQL_USER": "backup", "MYSQL_BIN": exe, "FAKE_DUMP": mode, "S3_ENDPOINT": server.URL, "S3_BUCKET": "backups", "S3_PREFIX": "mysql", "AWS_ACCESS_KEY_ID": "test", "AWS_SECRET_ACCESS_KEY": "testsecret", "STARTUP_CHECK_TIMEOUT": "10s"} {
				t.Setenv(k, v)
			}
			if mode == "timeout" {
				t.Setenv("STARTUP_CHECK_TIMEOUT", "1ns")
			}
			err = startupCheck(context.Background())
			if mode == "ok" || mode == "delete-denied" {
				if err != nil {
					t.Fatal(err)
				}
				if puts.Load() != 1 || deletes.Load() != 1 {
					t.Fatal("expected upload and cleanup")
				}
			} else {
				if err == nil {
					t.Fatal("expected startup failure")
				}
				if mode == "s3-denied" {
					if puts.Load() != 1 || deletes.Load() != 0 {
						t.Fatal("unexpected failed upload behavior")
					}
				} else if puts.Load() != 0 || deletes.Load() != 0 {
					t.Fatal("S3 was contacted after MySQL check failed")
				}
			}
		})
	}
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
	for _, mode := range []string{"ok", "fail", "partial", "discover"} {
		t.Run(mode, func(t *testing.T) {
			var uploads atomic.Int32
			var uploaded sync.Map
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
				segments := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
				if len(segments) != 4 || len(segments[2]) != 8 {
					t.Errorf("unexpected backup path: %s", r.URL.Path)
				}
				database := strings.SplitN(segments[len(segments)-1], "-", 2)[0]
				data, err := io.ReadAll(reader)
				if err != nil || string(data) != "CREATE DATABASE "+database+";\n" {
					t.Errorf("bad dump: %q %v", data, err)
				}
				uploads.Add(1)
				if _, exists := uploaded.LoadOrStore(database, true); exists {
					t.Errorf("duplicate upload for %s", database)
				}
				w.Header().Set("ETag", `"test"`)
				w.WriteHeader(200)
			}))
			defer server.Close()
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range map[string]string{"MYSQL_HOST": "localhost", "MYSQL_USER": "backup", "S3_ENDPOINT": server.URL, "S3_BUCKET": "backups", "AWS_ACCESS_KEY_ID": "test", "AWS_SECRET_ACCESS_KEY": "testsecret", "MYSQLDUMP_BIN": exe, "MYSQL_BIN": exe, "FAKE_DUMP": mode, "MYSQL_DATABASES": "app1 app2 app3 app1", "S3_PREFIX": "mysql"} {
				t.Setenv(k, v)
			}
			if mode == "discover" {
				t.Setenv("MYSQL_DATABASES", "")
			}
			err = run(context.Background())
			if mode == "ok" || mode == "discover" {
				if err != nil {
					t.Fatal(err)
				}
				if uploads.Load() != 3 {
					t.Fatal("expected three separate database uploads")
				}
				if mode == "discover" {
					if _, ok := uploaded.Load("mysql"); !ok {
						t.Fatal("mysql schema not backed up")
					}
				}
			} else {
				if err == nil {
					t.Fatal("expected dump error")
				}
				want := int32(0)
				if mode == "partial" {
					want = 2
				}
				if uploads.Load() != want {
					t.Fatalf("expected %d successful database uploads, got %d", want, uploads.Load())
				}
				if mode == "partial" {
					if _, ok := uploaded.Load("app2"); ok {
						t.Fatal("failed dump was uploaded")
					}
					if _, ok := uploaded.Load("app3"); !ok {
						t.Fatal("did not continue after database failure")
					}
				}
			}
		})
	}
}
