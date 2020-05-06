// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package boltdb

import (
	"bytes"
	"context"
	"sync/atomic"
	"time"

	"github.com/spacemonkeygo/monkit/v3"
	"github.com/zeebo/errs"
	"go.etcd.io/bbolt"

	"storj.io/storj/storage"
)

var mon = monkit.Package()

// Error is the default boltdb errs class
var Error = errs.Class("boltdb error")

// Client is the entrypoint into a bolt data store
type Client struct {
	db     *bbolt.DB
	Path   string
	Bucket []byte

	referenceCount *int32
	lookupLimit    int
}

const (
	// fileMode sets permissions so owner can read and write
	fileMode       = 0600
	defaultTimeout = 1 * time.Second
)

// New instantiates a new BoltDB client given db file path, and a bucket name
func New(path, bucket string) (*Client, error) {
	db, err := bbolt.Open(path, fileMode, &bbolt.Options{Timeout: defaultTimeout})
	if err != nil {
		return nil, Error.Wrap(err)
	}

	err = Error.Wrap(db.Update(func(tx *bbolt.Tx) error {
		_, err = tx.CreateBucketIfNotExists([]byte(bucket))
		return err
	}))
	if err != nil {
		if closeErr := Error.Wrap(db.Close()); closeErr != nil {
			return nil, errs.Combine(err, closeErr)
		}
		return nil, err
	}

	refCount := new(int32)
	*refCount = 1

	return &Client{
		db:             db,
		referenceCount: refCount,
		Path:           path,
		Bucket:         []byte(bucket),
		lookupLimit:    storage.DefaultLookupLimit,
	}, nil
}

// SetLookupLimit sets the lookup limit.
func (client *Client) SetLookupLimit(v int) { client.lookupLimit = v }

// LookupLimit returns the maximum limit that is allowed.
func (client *Client) LookupLimit() int { return client.lookupLimit }

func (client *Client) update(fn func(*bbolt.Bucket) error) error {
	return Error.Wrap(client.db.Update(func(tx *bbolt.Tx) error {
		return fn(tx.Bucket(client.Bucket))
	}))
}

func (client *Client) batch(fn func(*bbolt.Bucket) error) error {
	return Error.Wrap(client.db.Batch(func(tx *bbolt.Tx) error {
		return fn(tx.Bucket(client.Bucket))
	}))
}

func (client *Client) view(fn func(*bbolt.Bucket) error) error {
	return Error.Wrap(client.db.View(func(tx *bbolt.Tx) error {
		return fn(tx.Bucket(client.Bucket))
	}))
}

// Put adds a key/value to boltDB in a batch, where boltDB commits the batch to disk every
// 1000 operations or 10ms, whichever is first. The MaxBatchDelay are using default settings.
// Ref: https://github.com/boltdb/bolt/blob/master/db.go#L160
// Note: when using this method, check if it need to be executed asynchronously
// since it blocks for the duration db.MaxBatchDelay.
func (client *Client) Put(ctx context.Context, key storage.Key, value storage.Value) (err error) {
	defer mon.Task()(&ctx)(&err)
	start := time.Now()
	if key.IsZero() {
		return storage.ErrEmptyKey.New("")
	}

	err = client.batch(func(bucket *bbolt.Bucket) error {
		return bucket.Put(key, value)
	})
	mon.IntVal("boltdb_batch_time_elapsed").Observe(int64(time.Since(start)))
	return err
}

// PutAndCommit adds a key/value to BoltDB and writes it to disk.
func (client *Client) PutAndCommit(ctx context.Context, key storage.Key, value storage.Value) (err error) {
	defer mon.Task()(&ctx)(&err)
	if key.IsZero() {
		return storage.ErrEmptyKey.New("")
	}

	return client.update(func(bucket *bbolt.Bucket) error {
		return bucket.Put(key, value)
	})
}

// Get looks up the provided key from boltdb returning either an error or the result.
func (client *Client) Get(ctx context.Context, key storage.Key) (_ storage.Value, err error) {
	defer mon.Task()(&ctx)(&err)
	if key.IsZero() {
		return nil, storage.ErrEmptyKey.New("")
	}

	var value storage.Value
	err = client.view(func(bucket *bbolt.Bucket) error {
		data := bucket.Get([]byte(key))
		if len(data) == 0 {
			return storage.ErrKeyNotFound.New("%q", key)
		}
		value = storage.CloneValue(storage.Value(data))
		return nil
	})
	return value, err
}

// Delete deletes a key/value pair from boltdb, for a given the key
func (client *Client) Delete(ctx context.Context, key storage.Key) (err error) {
	defer mon.Task()(&ctx)(&err)
	if key.IsZero() {
		return storage.ErrEmptyKey.New("")
	}

	return client.update(func(bucket *bbolt.Bucket) error {
		return bucket.Delete(key)
	})
}

// DeleteMultiple deletes keys ignoring missing keys
func (client *Client) DeleteMultiple(ctx context.Context, keys []storage.Key) (_ storage.Items, err error) {
	defer mon.Task()(&ctx, len(keys))(&err)

	var items storage.Items
	err = client.update(func(bucket *bbolt.Bucket) error {
		for _, key := range keys {
			value := bucket.Get(key)
			if len(value) == 0 {
				continue
			}

			items = append(items, storage.ListItem{
				Key:   key,
				Value: value,
			})

			err := bucket.Delete(key)
			if err != nil {
				return err
			}
		}
		return nil
	})

	return items, err
}

// List returns either a list of keys for which boltdb has values or an error.
func (client *Client) List(ctx context.Context, first storage.Key, limit int) (_ storage.Keys, err error) {
	defer mon.Task()(&ctx)(&err)
	rv, err := storage.ListKeys(ctx, client, first, limit)
	return rv, Error.Wrap(err)
}

// Close closes a BoltDB client
func (client *Client) Close() (err error) {
	if atomic.AddInt32(client.referenceCount, -1) == 0 {
		return Error.Wrap(client.db.Close())
	}
	return nil
}

// GetAll finds all values for the provided keys (up to LookupLimit).
// If more keys are provided than the maximum, an error will be returned.
func (client *Client) GetAll(ctx context.Context, keys storage.Keys) (_ storage.Values, err error) {
	defer mon.Task()(&ctx)(&err)
	if len(keys) > client.lookupLimit {
		return nil, storage.ErrLimitExceeded
	}

	vals := make(storage.Values, 0, len(keys))
	err = client.view(func(bucket *bbolt.Bucket) error {
		for _, key := range keys {
			val := bucket.Get([]byte(key))
			if val == nil {
				vals = append(vals, nil)
				continue
			}
			vals = append(vals, storage.CloneValue(storage.Value(val)))
		}
		return nil
	})
	return vals, err
}

// Iterate iterates over items based on opts.
func (client *Client) Iterate(ctx context.Context, opts storage.IterateOptions, fn func(context.Context, storage.Iterator) error) (err error) {
	defer mon.Task()(&ctx)(&err)

	if opts.Limit <= 0 || opts.Limit > client.lookupLimit {
		opts.Limit = client.lookupLimit
	}

	return client.IterateWithoutLookupLimit(ctx, opts, fn)
}

// IterateWithoutLookupLimit calls the callback with an iterator over the keys, but doesn't enforce default limit on opts.
func (client *Client) IterateWithoutLookupLimit(ctx context.Context, opts storage.IterateOptions, fn func(context.Context, storage.Iterator) error) (err error) {
	defer mon.Task()(&ctx)(&err)

	return client.view(func(bucket *bbolt.Bucket) error {
		var cursor advancer = forward{bucket.Cursor()}

		start := true
		lastPrefix := []byte{}
		wasPrefix := false

		return fn(ctx, storage.IteratorFunc(func(ctx context.Context, item *storage.ListItem) bool {
			var key, value []byte
			if start {
				key, value = cursor.PositionToFirst(opts.Prefix, opts.First)
				start = false
			} else {
				key, value = cursor.Advance()
			}

			if !opts.Recurse {
				// when non-recursive skip all items that have the same prefix
				if wasPrefix && bytes.HasPrefix(key, lastPrefix) {
					key, value = cursor.SkipPrefix(lastPrefix)
					wasPrefix = false
				}
			}

			if len(key) == 0 || !bytes.HasPrefix(key, opts.Prefix) {
				return false
			}

			if !opts.Recurse {
				// check whether the entry is a proper prefix
				if p := bytes.IndexByte(key[len(opts.Prefix):], storage.Delimiter); p >= 0 {
					key = key[:len(opts.Prefix)+p+1]
					lastPrefix = append(lastPrefix[:0], key...)

					item.Key = append(item.Key[:0], storage.Key(lastPrefix)...)
					item.Value = item.Value[:0]
					item.IsPrefix = true

					wasPrefix = true
					return true
				}
			}

			item.Key = append(item.Key[:0], storage.Key(key)...)
			item.Value = append(item.Value[:0], storage.Value(value)...)
			item.IsPrefix = false

			return true
		}))
	})
}

type advancer interface {
	PositionToFirst(prefix, first storage.Key) (key, value []byte)
	SkipPrefix(prefix storage.Key) (key, value []byte)
	Advance() (key, value []byte)
}

type forward struct {
	*bbolt.Cursor
}

func (cursor forward) PositionToFirst(prefix, first storage.Key) (key, value []byte) {
	if first.IsZero() || first.Less(prefix) {
		return cursor.Seek([]byte(prefix))
	}
	return cursor.Seek([]byte(first))
}

func (cursor forward) SkipPrefix(prefix storage.Key) (key, value []byte) {
	return cursor.Seek(storage.AfterPrefix(prefix))
}

func (cursor forward) Advance() (key, value []byte) {
	return cursor.Next()
}

// CompareAndSwap atomically compares and swaps oldValue with newValue
func (client *Client) CompareAndSwap(ctx context.Context, key storage.Key, oldValue, newValue storage.Value) (err error) {
	defer mon.Task()(&ctx)(&err)
	if key.IsZero() {
		return storage.ErrEmptyKey.New("")
	}

	return client.update(func(bucket *bbolt.Bucket) error {
		data := bucket.Get([]byte(key))
		if len(data) == 0 {
			if oldValue != nil {
				return storage.ErrKeyNotFound.New("%q", key)
			}

			if newValue == nil {
				return nil
			}

			return Error.Wrap(bucket.Put(key, newValue))
		}

		if !bytes.Equal(storage.Value(data), oldValue) {
			return storage.ErrValueChanged.New("%q", key)
		}

		if newValue == nil {
			return Error.Wrap(bucket.Delete(key))
		}

		return Error.Wrap(bucket.Put(key, newValue))
	})
}
