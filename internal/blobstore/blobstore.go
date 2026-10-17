// Package blobstore stores raw downloads and original files in S3-compatible
// object storage (Cloudflare R2 in production) or on local disk (development).
//
// Objects are content-addressed: the key is derived from the SHA-256 of the
// original bytes, so identical files are stored once and keys never change.
package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

var ErrNotFound = errors.New("blob not found")

// Store is the minimal object storage API the application needs.
type Store interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Get(ctx context.Context, key string) ([]byte, error)
	Exists(ctx context.Context, key string) (bool, error)
}

// Object describes stored content. SHA256 and Size refer to the original
// (uncompressed) bytes.
type Object struct {
	SHA256      string
	Key         string
	ContentType string
	Size        int64
	Compression string // "none" or "zstd"
}

// Key returns the content-addressed key for a hash.
func Key(sha string, compression string) string {
	k := fmt.Sprintf("raw/sha256/%s/%s/%s", sha[0:2], sha[2:4], sha)
	if compression == "zstd" {
		k += ".zst"
	}
	return k
}

func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

var (
	encoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))
	decoder, _ = zstd.NewReader(nil)
)

// PutContent stores data under its content hash. Text formats should be
// compressed; already-compressed formats such as PDF should not. Uploading is
// skipped when the object already exists.
func PutContent(ctx context.Context, s Store, data []byte, contentType string, compress bool) (Object, error) {
	obj := Object{
		SHA256:      Hash(data),
		ContentType: contentType,
		Size:        int64(len(data)),
		Compression: "none",
	}
	if compress {
		obj.Compression = "zstd"
	}
	obj.Key = Key(obj.SHA256, obj.Compression)

	exists, err := s.Exists(ctx, obj.Key)
	if err != nil {
		return obj, err
	}
	if exists {
		return obj, nil
	}
	body := data
	if compress {
		body = encoder.EncodeAll(data, nil)
	}
	if err := s.Put(ctx, obj.Key, body, contentType); err != nil {
		return obj, err
	}
	return obj, nil
}

// GetContent returns the original bytes of a stored object.
func GetContent(ctx context.Context, s Store, key, compression string) ([]byte, error) {
	body, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if compression == "zstd" {
		return decoder.DecodeAll(body, nil)
	}
	return body, nil
}
