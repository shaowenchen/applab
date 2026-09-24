// Package objectstore is the object storage AppLab keeps everything in.
//
// AppLab used to keep its state in SQLite on a volume and its source in bare git
// repositories on the same volume. That made it stateful: one replica, pinned to
// one node, with a disk to lose and a claim to resize. Everything it needs to
// remember now lives in a bucket instead, so a replica can be replaced at any
// moment and hold nothing.
//
// The interface is deliberately the four operations S3 offers — get, put,
// delete, list — rather than anything shaped like a filesystem. A filesystem
// abstraction would invite code that needs rename, append or locking, none of
// which object storage has, and the failure would show up as corruption rather
// than as a compile error.
//
// Two implementations: S3, which speaks the REST API directly, and Local, a
// directory, which exists for development and for the tests. They are held to
// the same contract, and the tests run against both.
package objectstore

import (
	"context"
	"errors"
	"io"
	"strings"
)

// ErrNotExist is returned by Get and Delete when the key is not there.
//
// S3 reports this as a 404 rather than as an empty body, and the distinction
// matters everywhere it is used: an app that does not exist and an app whose
// record is empty are different answers, and only one of them is an error.
var ErrNotExist = errors.New("objectstore: no such key")

// Object is one entry in a listing.
type Object struct {
	// Key is the full path within the bucket.
	Key string
	// Size is the object's length in bytes.
	Size int64
	// ETag is the object's entity tag, as the store reports it. It is used to
	// tell whether a write replaced what was there, and is not otherwise
	// interpreted: S3's form for a single-part upload is the MD5 in quotes, but
	// nothing here depends on that.
	ETag string
}

// Store is the whole of what AppLab asks of object storage.
//
// Every method takes a key rather than a path — "/" separated, no leading slash
// — and every method is safe to call concurrently. Nothing here is transactional
// and nothing pretends to be: two writers to one key are last-write-wins, which
// is why the layout under it is one writer per object (see the store package).
type Store interface {
	// Get returns the object's contents. The caller closes it.
	//
	// It returns ErrNotExist, not an empty reader, when the key is absent.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// GetBytes is Get for callers that want the whole object at once and would
	// otherwise write the same six lines.
	GetBytes(ctx context.Context, key string) ([]byte, error)

	// Put writes an object, replacing anything at that key.
	Put(ctx context.Context, key string, r io.Reader, size int64) error

	// PutBytes is Put for a caller that already has the bytes.
	PutBytes(ctx context.Context, key string, body []byte) error

	// Delete removes an object. Removing a key that is not there is not an
	// error: the callers that delete are removing a state, and "it is already
	// gone" is that state reached.
	Delete(ctx context.Context, key string) error

	// Exists reports whether a key is present.
	Exists(ctx context.Context, key string) (bool, error)

	// List returns every object under a prefix, in lexicographic key order.
	//
	// Lexicographic order is not an implementation detail — S3 guarantees it, and
	// the layouts above rely on it to make "newest first" a matter of sorting the
	// page rather than of reading every object.
	//
	// An empty prefix lists the whole bucket.
	List(ctx context.Context, prefix string) ([]Object, error)

	// String names the backend, for a log line or an error message. It never
	// contains a credential.
	String() string
}

// Key joins path segments into a key, dropping empty ones and any slashes at
// the edges.
//
// It is the one place keys are built, so a layout cannot end up with "//" in it
// from one call site and not another — which object storage tolerates but which
// makes two keys for one thing.
func Key(segments ...string) string {
	parts := make([]string, 0, len(segments))
	for _, segment := range segments {
		segment = strings.Trim(segment, "/")
		if segment != "" {
			parts = append(parts, segment)
		}
	}
	return strings.Join(parts, "/")
}
