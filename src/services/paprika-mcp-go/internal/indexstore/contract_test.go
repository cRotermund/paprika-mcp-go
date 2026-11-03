package indexstore

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"testing"
	"time"
)

// backend is one storage location that can be opened at different format
// versions and have raw bytes planted in it.
type backend struct {
	open func(version int) Store
	put  func(raw []byte)
}

// runContract runs the cases every Store implementation must satisfy.
func runContract(t *testing.T, newBackend func(t *testing.T) backend) {
	t.Helper()
	small := Snapshot{
		SavedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Payload: []byte(`[{"uid":"a"}]`),
	}
	big := Snapshot{SavedAt: small.SavedAt, Payload: payloadOf(t, 5000)}

	gzipOf := func(s string) []byte {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write([]byte(s))
		_ = zw.Close()
		return buf.Bytes()
	}
	whole := gzipOf(`{"version":1,"saved_at":"2026-01-02T03:04:05Z","payload":[1,2,3]}`)

	tests := []struct {
		name string
		run  func(t *testing.T, b backend)
	}{
		{
			"absent",
			func(t *testing.T, b backend) {
				_, found, err := b.open(1).Load(context.Background())
				if err != nil || found {
					t.Fatalf("Load = found %v, err %v; want absent and no error", found, err)
				}
			},
		},
		{
			"round trip",
			func(t *testing.T, b backend) {
				assertRoundTrip(t, b.open(1), small)
			},
		},
		{
			"overwrite",
			func(t *testing.T, b backend) {
				s := b.open(1)
				if err := s.Save(context.Background(), small); err != nil {
					t.Fatalf("first Save: %v", err)
				}
				next := Snapshot{
					SavedAt: small.SavedAt.Add(time.Hour),
					Payload: []byte(`[{"uid":"b"},{"uid":"c"}]`),
				}
				assertRoundTrip(t, s, next)
			},
		},
		{
			"5000 entries",
			func(t *testing.T, b backend) {
				assertRoundTrip(t, b.open(1), big)
			},
		},
		{
			"old format version",
			func(t *testing.T, b backend) {
				if err := b.open(1).Save(context.Background(), small); err != nil {
					t.Fatalf("Save: %v", err)
				}
				_, found, err := b.open(2).Load(context.Background())
				if !errors.Is(err, ErrVersionMismatch) || !errors.Is(err, ErrUnusable) || found {
					t.Fatalf("Load = found %v, err %v; want ErrVersionMismatch", found, err)
				}
			},
		},
		{
			"corrupt not gzip",
			func(t *testing.T, b backend) {
				b.put([]byte("this is not gzip"))
				assertCorrupt(t, b.open(1))
			},
		},
		{
			"corrupt empty",
			func(t *testing.T, b backend) {
				b.put(nil)
				assertCorrupt(t, b.open(1))
			},
		},
		{
			"corrupt truncated",
			func(t *testing.T, b backend) {
				b.put(whole[:len(whole)/2])
				assertCorrupt(t, b.open(1))
			},
		},
		{
			"corrupt gzip of non JSON",
			func(t *testing.T, b backend) {
				b.put(gzipOf("not json"))
				assertCorrupt(t, b.open(1))
			},
		},
		{
			"corrupt missing payload",
			func(t *testing.T, b backend) {
				b.put(gzipOf(`{"version":1,"saved_at":"2026-01-02T03:04:05Z"}`))
				assertCorrupt(t, b.open(1))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newBackend(t))
		})
	}
}

func assertRoundTrip(t *testing.T, s Store, want Snapshot) {
	t.Helper()
	ctx := context.Background()
	if err := s.Save(ctx, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, found, err := s.Load(ctx)
	if err != nil || !found {
		t.Fatalf("Load = found %v, err %v; want found", found, err)
	}
	if !bytes.Equal(got.Payload, want.Payload) {
		t.Errorf(
			"payload differs: got %d bytes, want %d bytes",
			len(got.Payload),
			len(want.Payload),
		)
	}
	if !got.SavedAt.Equal(want.SavedAt) {
		t.Errorf("SavedAt = %v, want %v", got.SavedAt, want.SavedAt)
	}
	if got.Version != 1 {
		t.Errorf("Version = %d, want 1", got.Version)
	}
}

func assertCorrupt(t *testing.T, s Store) {
	t.Helper()
	_, found, err := s.Load(context.Background())
	if !errors.Is(err, ErrCorrupt) || !errors.Is(err, ErrUnusable) || found {
		t.Fatalf("Load = found %v, err %v; want ErrCorrupt", found, err)
	}
}
