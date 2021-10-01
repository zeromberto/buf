// Copyright 2020-2021 Buf Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package storagemem implements an in-memory storage Bucket.
package storagemem

import (
	"errors"

	"github.com/bufbuild/buf/private/pkg/storage"
	"github.com/bufbuild/buf/private/pkg/storage/storagemem/internal"
	"github.com/bufbuild/buf/private/pkg/storage/storageutil"
)

var errDuplicatePath = errors.New("duplicate path")

// NewReadWriteBucket returns a new in-memory ReadWriteBucket.
func NewReadWriteBucket(options ...ReadWriteBucketOption) storage.ReadWriteBucket {
	return newBucket(nil, options...)
}

// ReadWriteBucketOption is an option for any bucket returned from storagemem.
type ReadWriteBucketOption func(*bucket)

// ReadWriteBucketWithCompression returns a new ReadWriteBucketOption that enables compression.
//
// This will result in the stored data in buckets being compressed at rest.
// The data is automatically decompressed on read.
//
// The default is to use no compression.
func ReadWriteBucketWithCompression() ReadWriteBucketOption {
	return func(bucket *bucket) {
		bucket.compression = true
	}
}

// NewReadBucket returns a new ReadBucket.
func NewReadBucket(pathToData map[string][]byte) (storage.ReadBucket, error) {
	pathToImmutableObject := make(map[string]*internal.ImmutableObject, len(pathToData))
	for path, data := range pathToData {
		path, err := storageutil.ValidatePath(path)
		if err != nil {
			return nil, err
		}
		// This could happen if two paths normalize to the same path.
		if _, ok := pathToImmutableObject[path]; ok {
			return nil, errDuplicatePath
		}
		pathToImmutableObject[path] = internal.NewImmutableObject(path, "", data)
	}
	return newBucket(
		pathToImmutableObject,
		// TODO: remove this when we revert compression default to false
		func(bucket *bucket) {
			bucket.compression = false
		},
	), nil
}
