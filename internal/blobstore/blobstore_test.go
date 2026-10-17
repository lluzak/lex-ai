package blobstore

import (
	"context"
	"errors"
	"testing"
)

func TestPutContentRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	data := []byte(`{"title":"Ustawa z dnia 23 kwietnia 1964 r. Kodeks cywilny"}`)
	obj, err := PutContent(ctx, s, data, "application/json", true)
	if err != nil {
		t.Fatal(err)
	}
	if obj.Compression != "zstd" || obj.Size != int64(len(data)) {
		t.Fatalf("unexpected object %+v", obj)
	}
	if want := Key(obj.SHA256, "zstd"); obj.Key != want {
		t.Fatalf("key = %s, want %s", obj.Key, want)
	}

	got, err := GetContent(ctx, s, obj.Key, obj.Compression)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("round trip mismatch: %q", got)
	}

	// Same content again is a no-op with the same key.
	again, err := PutContent(ctx, s, data, "application/json", true)
	if err != nil || again.Key != obj.Key {
		t.Fatalf("second put: %v %+v", err, again)
	}
}

func TestGetMissing(t *testing.T) {
	s, _ := NewLocalFS(t.TempDir())
	if _, err := s.Get(context.Background(), "raw/sha256/aa/bb/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
