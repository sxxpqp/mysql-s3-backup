package main

import (
	"compress/gzip"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func run(ctx context.Context) error {
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
	// Do not echo client stderr: it may contain credentials or database content.
	if err := cmd.Run(); err != nil {
		_ = gz.Close()
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
	key := path.Join(env("S3_PREFIX", "mysql"), time.Now().UTC().Format("2006/01/02/20060102T150405.000000000Z")+".sql.gz")
	_, err = client.PutObject(ctx, os.Getenv("S3_BUCKET"), key, file, size, minio.PutObjectOptions{ContentType: "application/gzip"})
	if err != nil {
		return fmt.Errorf("upload backup: %w", err)
	}
	log.Printf("backup uploaded: s3://%s/%s (%d bytes)", os.Getenv("S3_BUCKET"), key, size)
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
