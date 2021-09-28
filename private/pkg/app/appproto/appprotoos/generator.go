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

package appprotoos

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bufbuild/buf/private/pkg/app"
	"github.com/bufbuild/buf/private/pkg/app/appproto"
	"github.com/bufbuild/buf/private/pkg/app/appproto/appprotoexec"
	"github.com/bufbuild/buf/private/pkg/normalpath"
	"github.com/bufbuild/buf/private/pkg/storage"
	"github.com/bufbuild/buf/private/pkg/storage/storagearchive"
	"github.com/bufbuild/buf/private/pkg/storage/storagemem"
	"github.com/bufbuild/buf/private/pkg/storage/storageos"
	"go.uber.org/multierr"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/pluginpb"
)

// Constants used to create .jar files
var (
	ManifestPath    = normalpath.Join("META-INF", "MANIFEST.MF")
	ManifestContent = []byte(`Manifest-Version: 1.0
Created-By: 1.6.0 (protoc)

`)
)

type generator struct {
	logger            *zap.Logger
	storageosProvider storageos.Provider
}

func newGenerator(
	logger *zap.Logger,
	storageosProvider storageos.Provider,
) *generator {
	return &generator{
		logger:            logger,
		storageosProvider: storageosProvider,
	}
}

func (g *generator) Generate(
	ctx context.Context,
	container app.EnvStderrContainer,
	pluginName string,
	pluginOut string,
	requests []*pluginpb.CodeGeneratorRequest,
	options ...GenerateOption,
) (_ *pluginpb.CodeGeneratorResponse, retErr error) {
	generateOptions := newGenerateOptions()
	for _, option := range options {
		option(generateOptions)
	}
	handler, err := appprotoexec.NewHandler(
		g.logger,
		g.storageosProvider,
		pluginName,
		appprotoexec.HandlerWithPluginPath(generateOptions.pluginPath),
	)
	if err != nil {
		return nil, err
	}
	var response *pluginpb.CodeGeneratorResponse
	generateFunc := func(writeBucket storage.WriteBucket) error {
		var generateOptions []appproto.GenerateOption
		if readBucket, ok := writeBucket.(storage.ReadBucket); ok {
			// We have to manually assert that we have a ReadBucket
			// since the storagemem.ReadBucketBuilder doesn't implement
			// the storage.ReadBucket interface.
			//
			// Insertion points aren't supported for .jar and .zip results.
			// If we were to support this, we would need to write everything
			// to a temporary directory, or support reading from a partially
			// computed storage.ReadBucket.
			//
			// For example,
			//
			//  # buf.gen.yaml
			//  version: v1
			//  plugins:
			//    - name: insertion-point-receiver
			//      out: foo.zip
			//    - name: insertion-point-writer
			//      out: bar.zip
			generateOptions = append(
				generateOptions,
				appproto.GenerateWithInsertionPointReadBucket(readBucket),
			)
		}
		// If successful, we return the the CodeGeneratorResponse created here.
		response, err = appproto.NewGenerator(g.logger, handler).Generate(
			ctx,
			container,
			writeBucket,
			requests,
			generateOptions...,
		)
		return err
	}
	if err := g.generate(
		ctx,
		generateFunc,
		pluginOut,
		generateOptions.createOutDirIfNotExists,
	); err != nil {
		return nil, err
	}
	return response, nil
}

func (g *generator) GenerateWithResponse(
	ctx context.Context,
	pluginOut string,
	response *pluginpb.CodeGeneratorResponse,
	options ...GenerateOption,
) (retErr error) {
	generateOptions := newGenerateOptions()
	for _, option := range options {
		option(generateOptions)
	}
	appprotoGenerator := appproto.NewGenerator(g.logger, nil /* We don't use a handler here */)
	generateFunc := func(writeBucket storage.WriteBucket) error {
		var generateOptions []appproto.GenerateOption
		if readBucket, ok := writeBucket.(storage.ReadBucket); ok {
			// We have to manually assert that we have a ReadBucket
			// since the storagemem.ReadBucketBuilder doesn't implement
			// the storage.ReadBucket interface.
			//
			// Insertion points aren't supported for .jar and .zip results.
			// If we were to support this, we would need to write everything
			// to a temporary directory, or support reading from a partially
			// computed storage.ReadBucket before it's actually written as
			// an archive.
			//
			// For example,
			//
			//  # buf.gen.yaml
			//  version: v1
			//  plugins:
			//    - name: insertion-point-receiver
			//      out: foo.zip
			//    - name: insertion-point-writer
			//      out: bar.zip
			generateOptions = append(
				generateOptions,
				appproto.GenerateWithInsertionPointReadBucket(readBucket),
			)
		}
		return appprotoGenerator.GenerateWithResponse(
			ctx,
			writeBucket,
			response,
			generateOptions...,
		)
	}
	return g.generate(
		ctx,
		generateFunc,
		pluginOut,
		generateOptions.createOutDirIfNotExists,
	)
}

func (g *generator) generate(
	ctx context.Context,
	generateFunc func(storage.WriteBucket) error,
	pluginOut string,
	createOutDirIfNotExists bool,
) error {
	switch filepath.Ext(pluginOut) {
	case ".jar":
		return g.generateZip(
			ctx,
			generateFunc,
			pluginOut,
			true,
			createOutDirIfNotExists,
		)
	case ".zip":
		return g.generateZip(
			ctx,
			generateFunc,
			pluginOut,
			false,
			createOutDirIfNotExists,
		)
	default:
		return g.generateDirectory(
			ctx,
			generateFunc,
			pluginOut,
			createOutDirIfNotExists,
		)
	}
}

func (g *generator) generateZip(
	ctx context.Context,
	generateFunc func(storage.WriteBucket) error,
	outFilePath string,
	includeManifest bool,
	createOutDirIfNotExists bool,
) (retErr error) {
	outDirPath := filepath.Dir(outFilePath)
	// OK to use os.Stat instead of os.Lstat here
	fileInfo, err := os.Stat(outDirPath)
	if err != nil {
		if os.IsNotExist(err) {
			if createOutDirIfNotExists {
				if err := os.MkdirAll(outDirPath, 0755); err != nil {
					return err
				}
			} else {
				return err
			}
		}
		return err
	} else if !fileInfo.IsDir() {
		return fmt.Errorf("not a directory: %s", outDirPath)
	}
	readBucketBuilder := storagemem.NewReadBucketBuilder()
	if err := generateFunc(readBucketBuilder); err != nil {
		return err
	}
	if includeManifest {
		if err := storage.PutPath(ctx, readBucketBuilder, ManifestPath, ManifestContent); err != nil {
			return err
		}
	}
	file, err := os.Create(outFilePath)
	if err != nil {
		return err
	}
	defer func() {
		retErr = multierr.Append(retErr, file.Close())
	}()
	readBucket, err := readBucketBuilder.ToReadBucket()
	if err != nil {
		return err
	}
	// protoc does not compress
	return storagearchive.Zip(ctx, readBucket, file, false)
}

func (g *generator) generateDirectory(
	ctx context.Context,
	generateFunc func(storage.WriteBucket) error,
	outDirPath string,
	createOutDirIfNotExists bool,
) error {
	if createOutDirIfNotExists {
		if err := os.MkdirAll(outDirPath, 0755); err != nil {
			return err
		}
	}
	// this checks that the directory exists
	readWriteBucket, err := g.storageosProvider.NewReadWriteBucket(
		outDirPath,
		storageos.ReadWriteBucketWithSymlinksIfSupported(),
	)
	if err != nil {
		return err
	}
	return generateFunc(readWriteBucket)
}

type generateOptions struct {
	pluginPath              string
	createOutDirIfNotExists bool
}

func newGenerateOptions() *generateOptions {
	return &generateOptions{}
}
