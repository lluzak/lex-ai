// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL string

	// HTTP
	Addr            string   // plain HTTP listen address, e.g. ":8080"
	TLSCertFile     string   // Cloudflare Origin CA certificate (optional)
	TLSKeyFile      string   // private key for TLSCertFile
	AutocertDomains []string // Let's Encrypt fallback when not behind Cloudflare
	AutocertDir     string
	BaseURL         string // public URL of the site, used in links
	FilesBaseURL    string // public R2 custom domain for original PDFs (optional)

	// Blob storage
	BlobDriver     string // "s3" or "localfs"
	BlobLocalDir   string
	S3Endpoint     string // e.g. <account_id>.r2.cloudflarestorage.com
	S3Region       string // "auto" for R2
	S3Bucket       string
	S3AccessKey    string
	S3SecretKey    string
	S3UseSSL       bool
	S3UsePathStyle bool

	// Ingestion
	SejmELIBaseURL  string
	UserAgent       string
	SourceRateLimit float64 // requests per second per source host
	Workers         int     // max concurrent jobs in the default queue
	DeltaInterval   time.Duration
	RunWorkers      bool
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL:     env("DATABASE_URL", "postgres://lexai:lexai@localhost:5432/lexai?sslmode=disable"),
		Addr:            env("LEXAI_ADDR", ":8080"),
		TLSCertFile:     os.Getenv("LEXAI_TLS_CERT"),
		TLSKeyFile:      os.Getenv("LEXAI_TLS_KEY"),
		AutocertDomains: list(os.Getenv("LEXAI_AUTOCERT_DOMAINS")),
		AutocertDir:     env("LEXAI_AUTOCERT_DIR", "var/autocert"),
		BaseURL:         strings.TrimRight(env("LEXAI_BASE_URL", "http://localhost:8080"), "/"),
		FilesBaseURL:    strings.TrimRight(os.Getenv("LEXAI_FILES_BASE_URL"), "/"),

		BlobDriver:     env("LEXAI_BLOB_DRIVER", "localfs"),
		BlobLocalDir:   env("LEXAI_BLOB_DIR", "var/files"),
		S3Endpoint:     os.Getenv("LEXAI_S3_ENDPOINT"),
		S3Region:       env("LEXAI_S3_REGION", "auto"),
		S3Bucket:       env("LEXAI_S3_BUCKET", "lexai-files"),
		S3AccessKey:    os.Getenv("LEXAI_S3_ACCESS_KEY"),
		S3SecretKey:    os.Getenv("LEXAI_S3_SECRET_KEY"),
		S3UseSSL:       envBool("LEXAI_S3_SSL", true),
		S3UsePathStyle: envBool("LEXAI_S3_PATH_STYLE", false),

		SejmELIBaseURL: strings.TrimRight(env("LEXAI_SEJM_ELI_URL", "https://api.sejm.gov.pl/eli"), "/"),
		UserAgent:      env("LEXAI_USER_AGENT", "lex-ai/0.1 (+https://github.com/lluzak/lex-ai)"),
		RunWorkers:     envBool("LEXAI_RUN_WORKERS", true),
	}
	var err error
	if c.SourceRateLimit, err = envFloat("LEXAI_SOURCE_RPS", 3); err != nil {
		return c, err
	}
	if c.Workers, err = envInt("LEXAI_WORKERS", 8); err != nil {
		return c, err
	}
	if c.DeltaInterval, err = envDuration("LEXAI_DELTA_INTERVAL", 24*time.Hour); err != nil {
		return c, err
	}
	if c.BlobDriver != "s3" && c.BlobDriver != "localfs" {
		return c, fmt.Errorf("LEXAI_BLOB_DRIVER must be s3 or localfs, got %q", c.BlobDriver)
	}
	if c.BlobDriver == "s3" && (c.S3Endpoint == "" || c.S3AccessKey == "" || c.S3SecretKey == "") {
		return c, fmt.Errorf("LEXAI_S3_ENDPOINT, LEXAI_S3_ACCESS_KEY and LEXAI_S3_SECRET_KEY are required for the s3 blob driver")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return c, fmt.Errorf("LEXAI_TLS_CERT and LEXAI_TLS_KEY must be set together")
	}
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func list(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func envFloat(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return f, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}
