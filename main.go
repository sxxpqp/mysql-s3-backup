package main

import (
	"compress/gzip"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/robfig/cron/v3"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Timestamp every log in the scheduling timezone, including its UTC offset.
type timezoneLogWriter struct {
	output   io.Writer
	location *time.Location
}

func (w timezoneLogWriter) Write(p []byte) (int, error) {
	_, err := fmt.Fprintf(w.output, "%s %s", time.Now().In(w.location).Format(time.RFC3339), p)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func run(ctx context.Context) (result error) {
	started := time.Now()
	defer func() {
		if result != nil {
			log.Printf("backup failed: elapsed=%s error=%v", time.Since(started).Round(time.Millisecond), result)
		} else {
			log.Printf("backup completed: elapsed=%s", time.Since(started).Round(time.Millisecond))
		}
	}()
	if err := validateConfig(); err != nil {
		return err
	}
	timeout, err := time.ParseDuration(env("BACKUP_TIMEOUT", "2h"))
	if err != nil || timeout <= 0 {
		return fmt.Errorf("invalid BACKUP_TIMEOUT")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	log.Printf("backup started: mysql=%q port=%q databases=%q timeout=%s", os.Getenv("MYSQL_HOST"), env("MYSQL_PORT", "3306"), env("MYSQL_DATABASES", "ALL"), timeout)
	client, err := newS3Client()
	if err != nil {
		return err
	}
	databases, err := listDatabases(ctx)
	if err != nil {
		return err
	}
	if len(databases) == 0 {
		return fmt.Errorf("no databases available for backup")
	}
	log.Printf("database list: count=%d names=%q", len(databases), databases)
	var failures []error
	succeeded := 0
	failed := 0
	location, err := time.LoadLocation(env("BACKUP_TIMEZONE", "Asia/Shanghai"))
	if err != nil {
		return fmt.Errorf("invalid BACKUP_TIMEZONE: %w", err)
	}
	roundTime := started.In(location)
	for i, database := range databases {
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
			break
		}
		log.Printf("database backup started: database=%q progress=%d/%d", database, i+1, len(databases))
		if err := backupDatabase(ctx, client, database, roundTime); err != nil {
			failed++
			log.Printf("database backup failed: database=%q error=%v", database, err)
			failures = append(failures, fmt.Errorf("database %q: %w", database, err))
		} else {
			succeeded++
		}
	}
	log.Printf("backup summary: total=%d succeeded=%d failed=%d skipped=%d", len(databases), succeeded, failed, len(databases)-succeeded-failed)
	return errors.Join(failures...)
}

func validateConfig() error {
	for _, key := range []string{"MYSQL_HOST", "MYSQL_USER", "S3_ENDPOINT", "S3_BUCKET", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if os.Getenv(key) == "" {
			return fmt.Errorf("required environment variable: %s", key)
		}
	}
	if duration, err := time.ParseDuration(env("BACKUP_TIMEOUT", "2h")); err != nil || duration <= 0 {
		return fmt.Errorf("invalid BACKUP_TIMEOUT")
	}
	return nil
}

func newS3Client() (*minio.Client, error) {
	endpoint := os.Getenv("S3_ENDPOINT")
	secure := true
	if strings.HasPrefix(endpoint, "http://") {
		secure = false
		endpoint = strings.TrimPrefix(endpoint, "http://")
	} else {
		endpoint = strings.TrimPrefix(endpoint, "https://")
	}
	client, err := minio.New(strings.TrimSuffix(endpoint, "/"), &minio.Options{Creds: credentials.NewStaticV4(os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), os.Getenv("AWS_SESSION_TOKEN")), Secure: secure, Region: env("AWS_REGION", "us-east-1"), BucketLookup: minio.BucketLookupPath})
	if err != nil {
		return nil, fmt.Errorf("S3 configuration: %w", err)
	}
	return client, nil
}

func startupCheck(ctx context.Context) error {
	if err := validateConfig(); err != nil {
		return err
	}
	timeout, err := time.ParseDuration(env("STARTUP_CHECK_TIMEOUT", "30s"))
	if err != nil || timeout <= 0 {
		return fmt.Errorf("invalid STARTUP_CHECK_TIMEOUT")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	log.Printf("startup check: MySQL login and SELECT 1 host=%q port=%q timeout=%s", os.Getenv("MYSQL_HOST"), env("MYSQL_PORT", "3306"), timeout)
	args := append(connectionArgs(), "--batch", "--skip-column-names", "--execute=SELECT 1")
	cmd := exec.CommandContext(ctx, env("MYSQL_BIN", "mysql"), args...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+os.Getenv("MYSQL_PASSWORD"))
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return fmt.Errorf("startup MySQL check interrupted: %w", ctx.Err())
	}
	if err != nil {
		return fmt.Errorf("startup MySQL login/query failed: %w (check DNS, network, credentials and TLS)", err)
	}
	if strings.TrimSpace(string(output)) != "1" {
		return fmt.Errorf("startup MySQL SELECT 1 returned unexpected response")
	}
	log.Printf("startup MySQL check passed: elapsed=%s", time.Since(started).Round(time.Millisecond))
	client, err := newS3Client()
	if err != nil {
		return err
	}
	location, err := time.LoadLocation(env("BACKUP_TIMEZONE", "Asia/Shanghai"))
	if err != nil {
		return fmt.Errorf("invalid BACKUP_TIMEZONE: %w", err)
	}
	key := path.Join(env("S3_PREFIX", "mysql"), ".connection-checks", time.Now().In(location).Format("20060102T150405.000000000-0700")+".txt")
	log.Printf("startup check: S3 bucket upload destination=s3://%s/%s", os.Getenv("S3_BUCKET"), key)
	payload := "mysql-s3-backup startup connection check\n"
	if _, err := client.PutObject(ctx, os.Getenv("S3_BUCKET"), key, strings.NewReader(payload), int64(len(payload)), minio.PutObjectOptions{ContentType: "text/plain"}); err != nil {
		return fmt.Errorf("startup S3 upload check failed: %w", err)
	}
	log.Print("startup S3 upload check passed: bucket exists and credentials permit writing probe object")
	// Cleanup is best effort: backup accounts need not have delete permissions.
	if err := client.RemoveObject(ctx, os.Getenv("S3_BUCKET"), key, minio.RemoveObjectOptions{}); err != nil {
		log.Printf("WARNING startup probe cleanup failed: object=s3://%s/%s error=%v; remove it manually or use a lifecycle rule", os.Getenv("S3_BUCKET"), key, err)
	} else {
		log.Print("startup S3 probe cleanup passed")
	}
	if ctx.Err() != nil {
		return fmt.Errorf("startup check interrupted: %w", ctx.Err())
	}
	log.Printf("startup checks passed: elapsed=%s; ready to back up or enter schedule", time.Since(started).Round(time.Millisecond))
	return nil
}

func connectionArgs() []string {
	return []string{"--protocol=TCP", "--host=" + os.Getenv("MYSQL_HOST"), "--port=" + env("MYSQL_PORT", "3306"), "--user=" + os.Getenv("MYSQL_USER")}
}

func listDatabases(ctx context.Context) ([]string, error) {
	databases := strings.Fields(os.Getenv("MYSQL_DATABASES"))
	if len(databases) == 0 {
		log.Print("discovering databases: excluding virtual schemas information_schema, performance_schema, sys; including mysql")
		// Hex encoding preserves database names containing whitespace or newlines.
		args := append(connectionArgs(), "--batch", "--skip-column-names", "--raw", "--execute=SELECT HEX(SCHEMA_NAME) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME NOT IN ('information_schema','performance_schema','sys') ORDER BY SCHEMA_NAME")
		cmd := exec.CommandContext(ctx, env("MYSQL_BIN", "mysql"), args...)
		cmd.Env = append(os.Environ(), "MYSQL_PWD="+os.Getenv("MYSQL_PASSWORD"))
		output, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("list databases failed: %w", err)
		}
		for _, line := range strings.Fields(string(output)) {
			name, err := hex.DecodeString(line)
			if err != nil {
				return nil, fmt.Errorf("invalid database list response: %w", err)
			}
			databases = append(databases, string(name))
		}
	}
	seen := make(map[string]bool)
	unique := make([]string, 0, len(databases))
	for _, database := range databases {
		if database == "" || strings.HasPrefix(database, "-") || strings.ContainsRune(database, 0) {
			return nil, fmt.Errorf("invalid database name %q", database)
		}
		if !seen[database] {
			unique = append(unique, database)
			seen[database] = true
		}
	}
	return unique, nil
}

func backupObjectKey(prefix, database string, roundTime time.Time) string {
	return path.Join(prefix, roundTime.Format("20060102"), url.PathEscape(database)+"-"+roundTime.Format("20060102T150405.000000000-0700")+".sql.gz")
}

func backupDatabase(ctx context.Context, client *minio.Client, database string, roundTime time.Time) error {
	started := time.Now()
	// Only upload a complete, successful dump. Temporary backup is removed on every exit.
	file, err := os.CreateTemp("", "mysql-backup-*.sql.gz")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	args := append(connectionArgs(), "--single-transaction", "--quick", "--routines", "--events", "--triggers", "--hex-blob", "--no-tablespaces", "--set-gtid-purged=OFF", "--databases", database)
	cmd := exec.CommandContext(ctx, env("MYSQLDUMP_BIN", "mysqldump"), args...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+os.Getenv("MYSQL_PASSWORD"))
	gz := gzip.NewWriter(file)
	cmd.Stdout = gz
	dumpStarted := time.Now()
	log.Printf("dump started: database=%q mysqldump + gzip", database)
	// Do not echo client stderr: it may contain credentials or database content.
	if err := cmd.Run(); err != nil {
		_ = gz.Close()
		if ctx.Err() != nil {
			return fmt.Errorf("dump interrupted: %w", ctx.Err())
		}
		return fmt.Errorf("mysqldump failed: %w", err)
	}
	if err := gz.Close(); err != nil {
		return err
	}
	size, err := file.Seek(0, 2)
	if err != nil {
		return err
	}
	if _, err = file.Seek(0, 0); err != nil {
		return err
	}
	log.Printf("dump completed: database=%q elapsed=%s compressed_bytes=%d compressed_mib=%.2f", database, time.Since(dumpStarted).Round(time.Millisecond), size, float64(size)/(1024*1024))
	key := backupObjectKey(env("S3_PREFIX", "mysql"), database, roundTime)
	uploadStarted := time.Now()
	log.Printf("upload started: destination=s3://%s/%s bytes=%d", os.Getenv("S3_BUCKET"), key, size)
	_, err = client.PutObject(ctx, os.Getenv("S3_BUCKET"), key, file, size, minio.PutObjectOptions{ContentType: "application/gzip"})
	if err != nil {
		return fmt.Errorf("upload backup: %w", err)
	}
	log.Printf("backup uploaded: s3://%s/%s (%d bytes) upload_elapsed=%s", os.Getenv("S3_BUCKET"), key, size, time.Since(uploadStarted).Round(time.Millisecond))
	log.Printf("database backup completed: database=%q elapsed=%s", database, time.Since(started).Round(time.Millisecond))
	return nil
}

// Wait after each attempt so slow backups never overlap in this process.
func runScheduled(ctx context.Context, interval time.Duration, backup func(context.Context) error) {
	for ctx.Err() == nil {
		if err := backup(ctx); err != nil && ctx.Err() == nil {
			log.Printf("backup failed; retry after %s: %v", interval, err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func parseCron(value, zone string) (cron.Schedule, *time.Location, error) {
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid BACKUP_TIMEZONE: %w", err)
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedule, err := parser.Parse(value)
	if err != nil {
		return nil, nil, fmt.Errorf("BACKUP_INTERVAL must be a five-field cron expression or positive duration: %w", err)
	}
	if schedule.Next(time.Now().In(location)).IsZero() {
		return nil, nil, fmt.Errorf("BACKUP_INTERVAL has no upcoming execution")
	}
	return schedule, location, nil
}

func runCron(ctx context.Context, schedule cron.Schedule, location *time.Location, backup func(context.Context) error) {
	for ctx.Err() == nil {
		next := schedule.Next(time.Now().In(location))
		if next.IsZero() {
			log.Print("cron schedule has no upcoming execution")
			return
		}
		log.Printf("next backup: %s wait=%s", next.Format(time.RFC3339), time.Until(next).Round(time.Second))
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}
		if err := backup(ctx); err != nil && ctx.Err() == nil {
			log.Printf("backup failed; retry at next scheduled time: %v", err)
		}
		// Compute the next future time after completion; missed ticks are skipped.
	}
}

func main() {
	location, err := time.LoadLocation(env("BACKUP_TIMEZONE", "Asia/Shanghai"))
	if err != nil {
		log.Fatalf("invalid BACKUP_TIMEZONE: %v", err)
	}
	log.SetFlags(0)
	log.SetOutput(timezoneLogWriter{output: os.Stderr, location: location})
	log.Printf("service started: timezone=%s schedule=%q timeout=%q object_timestamps=%s", location, os.Getenv("BACKUP_INTERVAL"), env("BACKUP_TIMEOUT", "2h"), location)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := startupCheck(ctx); err != nil {
		log.Fatalf("startup checks failed: %v", err)
	}
	if value := os.Getenv("BACKUP_INTERVAL"); value != "" {
		interval, err := time.ParseDuration(value)
		if err != nil {
			schedule, location, err := parseCron(value, env("BACKUP_TIMEZONE", "Asia/Shanghai"))
			if err != nil {
				log.Fatal(err)
			}
			runCron(ctx, schedule, location, run)
			return
		}
		if interval <= 0 {
			log.Fatal("BACKUP_INTERVAL duration must be positive")
		}
		log.Printf("scheduled backup enabled: immediate first backup, then %s after each attempt", interval)
		runScheduled(ctx, interval, run)
		return
	}
	if err := run(ctx); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
