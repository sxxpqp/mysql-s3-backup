package main

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
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
	for _, key := range []string{"MYSQL_HOST", "MYSQL_USER", "S3_ENDPOINT", "S3_BUCKET", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if os.Getenv(key) == "" {
			return fmt.Errorf("required environment variable: %s", key)
		}
	}
	timeout, err := time.ParseDuration(env("BACKUP_TIMEOUT", "2h"))
	if err != nil || timeout <= 0 {
		return fmt.Errorf("invalid BACKUP_TIMEOUT")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	log.Printf("backup started: mysql=%q port=%q databases=%q timeout=%s", os.Getenv("MYSQL_HOST"), env("MYSQL_PORT", "3306"), env("MYSQL_DATABASES", "ALL"), timeout)
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
		return fmt.Errorf("S3 configuration: %w", err)
	}
	// Only upload a complete, successful dump. Temporary backup is removed on every exit.
	file, err := os.CreateTemp("", "mysql-backup-*.sql.gz")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	args := []string{"--host=" + os.Getenv("MYSQL_HOST"), "--port=" + env("MYSQL_PORT", "3306"), "--user=" + os.Getenv("MYSQL_USER"), "--single-transaction", "--quick", "--routines", "--events", "--triggers", "--hex-blob", "--no-tablespaces", "--set-gtid-purged=OFF"}
	if databases := strings.Fields(os.Getenv("MYSQL_DATABASES")); len(databases) > 0 {
		for _, db := range databases {
			if strings.HasPrefix(db, "-") {
				return fmt.Errorf("invalid database name")
			}
		}
		args = append(args, "--databases")
		args = append(args, databases...)
	} else {
		args = append(args, "--all-databases")
	}
	cmd := exec.CommandContext(ctx, env("MYSQLDUMP_BIN", "mysqldump"), args...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+os.Getenv("MYSQL_PASSWORD"))
	gz := gzip.NewWriter(file)
	cmd.Stdout = gz
	dumpStarted := time.Now()
	log.Print("dump started: mysqldump + gzip")
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
	log.Printf("dump completed: elapsed=%s compressed_bytes=%d compressed_mib=%.2f", time.Since(dumpStarted).Round(time.Millisecond), size, float64(size)/(1024*1024))
	key := path.Join(env("S3_PREFIX", "mysql"), time.Now().UTC().Format("2006/01/02/20060102T150405.000000000Z")+".sql.gz")
	uploadStarted := time.Now()
	log.Printf("upload started: destination=s3://%s/%s bytes=%d", os.Getenv("S3_BUCKET"), key, size)
	_, err = client.PutObject(ctx, os.Getenv("S3_BUCKET"), key, file, size, minio.PutObjectOptions{ContentType: "application/gzip"})
	if err != nil {
		return fmt.Errorf("upload backup: %w", err)
	}
	log.Printf("backup uploaded: s3://%s/%s (%d bytes) upload_elapsed=%s", os.Getenv("S3_BUCKET"), key, size, time.Since(uploadStarted).Round(time.Millisecond))
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
	log.Printf("service started: timezone=%s schedule=%q timeout=%q object_timestamps=UTC", location, os.Getenv("BACKUP_INTERVAL"), env("BACKUP_TIMEOUT", "2h"))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
