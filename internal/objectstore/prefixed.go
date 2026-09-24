package objectstore

import (
	"context"
	"io"
	"strings"
)

// prefixed is a store that writes every key under a prefix.
//
// It exists so one bucket can hold more than one AppLab deployment — a staging
// install beside a production one, or several environments of a platform built
// on AppLab. The alternative is a bucket each, which is more to create and more
// to grant access to.
//
// The prefix is applied on the way in and stripped on the way out, so nothing
// above this sees it: a caller lists "apps/" and gets apps, not
// "team-a/apps/". A store's layout should not have to know where it is mounted.
type prefixed struct {
	Store
	prefix string
}

// Prefixed returns a store whose keys all live under a prefix.
//
// An empty prefix returns the store unchanged rather than wrapping it, so the
// common case costs nothing and cannot be the source of a bug.
func Prefixed(inner Store, prefix string) Store {
	prefix = Key(prefix)
	if prefix == "" {
		return inner
	}
	return &prefixed{Store: inner, prefix: prefix}
}

func (p *prefixed) key(key string) string {
	clean := Key(key)
	if clean == "" {
		return p.prefix
	}
	return p.prefix + "/" + clean
}

// strip turns a key from the wrapped store back into one this store's callers
// would recognise. A key outside the prefix is left as it is, which can only
// happen if the underlying store returns something the caller did not ask for.
func (p *prefixed) strip(key string) string {
	if key == p.prefix {
		return ""
	}
	return strings.TrimPrefix(key, p.prefix+"/")
}

func (p *prefixed) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return p.Store.Get(ctx, p.key(key))
}

func (p *prefixed) GetBytes(ctx context.Context, key string) ([]byte, error) {
	return p.Store.GetBytes(ctx, p.key(key))
}

func (p *prefixed) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	return p.Store.Put(ctx, p.key(key), r, size)
}

func (p *prefixed) PutBytes(ctx context.Context, key string, body []byte) error {
	return p.Store.PutBytes(ctx, p.key(key), body)
}

func (p *prefixed) Delete(ctx context.Context, key string) error {
	return p.Store.Delete(ctx, p.key(key))
}

func (p *prefixed) Exists(ctx context.Context, key string) (bool, error) {
	return p.Store.Exists(ctx, p.key(key))
}

func (p *prefixed) List(ctx context.Context, prefix string) ([]Object, error) {
	// The listing is scoped to the mount point, so a caller listing "" sees its
	// own objects and not whatever else is in the bucket.
	objects, err := p.Store.List(ctx, p.key(prefix))
	if err != nil {
		return nil, err
	}

	out := make([]Object, 0, len(objects))
	for _, object := range objects {
		object.Key = p.strip(object.Key)
		out = append(out, object)
	}
	return out, nil
}

func (p *prefixed) String() string { return p.Store.String() + "/" + p.prefix }
