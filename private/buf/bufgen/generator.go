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

package bufgen

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/bufbuild/buf/private/bufpkg/bufimage"
	"github.com/bufbuild/buf/private/bufpkg/bufimage/bufimagemodify"
	"github.com/bufbuild/buf/private/bufpkg/bufplugin"
	"github.com/bufbuild/buf/private/gen/proto/apiclient/buf/alpha/registry/v1alpha1/registryv1alpha1apiclient"
	registryv1alpha1 "github.com/bufbuild/buf/private/gen/proto/go/buf/alpha/registry/v1alpha1"
	"github.com/bufbuild/buf/private/pkg/app"
	"github.com/bufbuild/buf/private/pkg/app/appproto/appprotoexec"
	"github.com/bufbuild/buf/private/pkg/app/appproto/appprotoos"
	"github.com/bufbuild/buf/private/pkg/storage/storageos"
	"github.com/bufbuild/buf/private/pkg/thread"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/pluginpb"
)

type generator struct {
	logger              *zap.Logger
	appprotoosGenerator appprotoos.Generator
	registryProvider    registryv1alpha1apiclient.Provider
}

func newGenerator(
	logger *zap.Logger,
	storageosProvider storageos.Provider,
	registryProvider registryv1alpha1apiclient.Provider,
) *generator {
	return &generator{
		logger:              logger,
		appprotoosGenerator: appprotoos.NewGenerator(logger, storageosProvider),
		registryProvider:    registryProvider,
	}
}

func (g *generator) Generate(
	ctx context.Context,
	container app.EnvStdioContainer,
	config *Config,
	image bufimage.Image,
	options ...GenerateOption,
) error {
	generateOptions := newGenerateOptions()
	for _, option := range options {
		option(generateOptions)
	}
	return g.generate(
		ctx,
		container,
		config,
		image,
		generateOptions.baseOutDirPath,
		generateOptions.includeImports,
	)
}

// generate executes all of the plugins specified by the given Config, and
// consolidates the results in the same order that the plugins are listed.
// Order is particularly important for insertion points, which are used to
// modify the generted output from other plugins executed earlier in the chain.
func (g *generator) generate(
	ctx context.Context,
	container app.EnvStdioContainer,
	config *Config,
	image bufimage.Image,
	baseOutDirPath string,
	includeImports bool,
) error {
	if err := modifyImage(ctx, g.logger, config, image); err != nil {
		return err
	}
	// Cache imagesByDir up-front if at least one plugin requires it
	var imagesByDir []bufimage.Image
	for _, pluginConfig := range config.PluginConfigs {
		if pluginConfig.Strategy != StrategyDirectory {
			continue
		}
		// Already guaranteed by config validation, but best to be safe.
		if pluginConfig.Remote != "" {
			return fmt.Errorf("remote plugin %q cannot set strategy directory", pluginConfig.Remote)
		}
		var err error
		imagesByDir, err = bufimage.ImageByDir(image)
		if err != nil {
			return err
		}
		break
	}
	// Collect all of the remote plugin jobs so that they can be executed in parallel.
	var remoteJobs []func(context.Context) error
	responses := make([]*pluginpb.CodeGeneratorResponse, len(config.PluginConfigs))
	for i, pluginConfig := range config.PluginConfigs {
		if pluginConfig.Remote != "" {
			index := i
			remotePluginConfig := pluginConfig
			remoteJobs = append(remoteJobs, func(ctx context.Context) error {
				response, err := g.execRemotePlugin(
					ctx,
					container,
					image,
					remotePluginConfig,
					// TODO: We need to support the includeImports option here.
				)
				if err != nil {
					return err
				}
				responses[index] = response
				return nil
			})
		}
	}
	// It's important that we execute all of the remote jobs first so that any insertion points
	// handled in local plugins can act upon the remote-generated files.
	//
	// For example,
	//
	//  # buf.gen.yaml
	//  version: v1
	//  plugins:
	//    - remote: buf.build/org/plugins/insertion-point-receiver
	//      out: gen/proto
	//    - name: insertion-point-writer
	//      out: gen/proto
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := thread.Parallelize(ctx, remoteJobs, thread.ParallelizeWithCancel(cancel)); err != nil {
		return err
	}
	// Execute and/or apply the CodeGeneratorResponses in the order they were specified.
	for i, pluginConfig := range config.PluginConfigs {
		out := pluginConfig.Out
		if baseOutDirPath != "" && baseOutDirPath != "." {
			out = filepath.Join(baseOutDirPath, out)
		}
		if pluginConfig.Remote != "" {
			// If this is a remote plugin, then we should expect a CodeGeneratorResponse
			// at the same index via the thread.Parallelize call above.
			response := responses[i]
			if response == nil {
				return fmt.Errorf("failed to get plugin response for %s", pluginConfig.Remote)
			}
			if err := g.appprotoosGenerator.GenerateWithResponse(
				ctx,
				out,
				response,
				appprotoos.GenerateWithCreateOutDirIfNotExists(),
			); err != nil {
				return fmt.Errorf("plugin %s: %v", pluginConfig.PluginName(), err)
			}
		}
		if pluginConfig.Name != "" {
			if responses[i] != nil {
				// This should be unreachable, but we include this check for additional safety.
				return fmt.Errorf("plugin %q already has a response", pluginConfig.Name)
			}
			response, err := g.execLocalPlugin(
				ctx,
				container,
				image,
				imagesByDir,
				pluginConfig,
				out,
				includeImports,
			)
			if err != nil {
				return err
			}
			responses[i] = response
		}
	}
	if err := validateResponses(responses, config.PluginConfigs); err != nil {
		return err
	}
	return nil
}

func (g *generator) execLocalPlugin(
	ctx context.Context,
	container app.EnvStdioContainer,
	image bufimage.Image,
	imagesByDir []bufimage.Image,
	pluginConfig *PluginConfig,
	out string,
	includeImports bool,
) (*pluginpb.CodeGeneratorResponse, error) {
	var pluginImages []bufimage.Image
	switch pluginConfig.Strategy {
	case StrategyAll:
		pluginImages = []bufimage.Image{image}
	case StrategyDirectory:
		pluginImages = imagesByDir
	default:
		return nil, fmt.Errorf("unknown strategy: %v", pluginConfig.Strategy)
	}
	response, err := g.appprotoosGenerator.Generate(
		ctx,
		container,
		pluginConfig.Name,
		out,
		bufimage.ImagesToCodeGeneratorRequests(
			pluginImages,
			pluginConfig.Opt,
			appprotoexec.DefaultVersion,
			includeImports,
		),
		appprotoos.GenerateWithPluginPath(pluginConfig.Path),
		appprotoos.GenerateWithCreateOutDirIfNotExists(),
	)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: %v", pluginConfig.PluginName(), err)
	}
	return response, nil
}

func (g *generator) execRemotePlugin(
	ctx context.Context,
	container app.EnvStdioContainer,
	image bufimage.Image,
	pluginConfig *PluginConfig,
) (*pluginpb.CodeGeneratorResponse, error) {
	remote, owner, name, version, err := bufplugin.ParsePluginVersionPath(pluginConfig.Remote)
	if err != nil {
		return nil, fmt.Errorf("invalid plugin path: %w", err)
	}
	generateService, err := g.registryProvider.NewGenerateService(ctx, remote)
	if err != nil {
		return nil, fmt.Errorf("failed to create generate service for remote %q: %w", remote, err)
	}
	var parameters []string
	if len(pluginConfig.Opt) > 0 {
		// Only include parameters if they're not empty.
		parameters = []string{pluginConfig.Opt}
	}
	pluginReference := &registryv1alpha1.PluginReference{
		Owner:      owner,
		Name:       name,
		Version:    version,
		Parameters: parameters,
	}
	responses, _, err := generateService.GeneratePlugins(
		ctx,
		bufimage.ImageToProtoImage(image),
		[]*registryv1alpha1.PluginReference{pluginReference},
	)
	if err != nil {
		return nil, err
	}
	if len(responses) != 1 {
		return nil, fmt.Errorf("unexpected number of responses received, got %d, wanted %d", len(responses), 1)
	}
	return responses[0], nil
}

// modifyImage modifies the image according to the given configuration (i.e. Managed Mode).
func modifyImage(
	ctx context.Context,
	logger *zap.Logger,
	config *Config,
	image bufimage.Image,
) error {
	if config.ManagedConfig == nil {
		// If the config is nil, it implies that the
		// user has not enabled managed mode.
		return nil
	}
	sweeper := bufimagemodify.NewFileOptionSweeper()
	modifier, err := newModifier(logger, config.ManagedConfig, sweeper)
	if err != nil {
		return err
	}
	modifier = bufimagemodify.Merge(modifier, bufimagemodify.ModifierFunc(sweeper.Sweep))
	return modifier.Modify(ctx, image)
}

func newModifier(
	logger *zap.Logger,
	managedConfig *ManagedConfig,
	sweeper bufimagemodify.Sweeper,
) (bufimagemodify.Modifier, error) {
	modifier := bufimagemodify.NewMultiModifier(
		bufimagemodify.JavaOuterClassname(logger, sweeper, managedConfig.Override[bufimagemodify.JavaOuterClassNameID]),
		bufimagemodify.ObjcClassPrefix(logger, sweeper, managedConfig.Override[bufimagemodify.ObjcClassPrefixID]),
		bufimagemodify.CsharpNamespace(logger, sweeper, managedConfig.Override[bufimagemodify.CsharpNamespaceID]),
		bufimagemodify.PhpNamespace(logger, sweeper, managedConfig.Override[bufimagemodify.PhpNamespaceID]),
		bufimagemodify.PhpMetadataNamespace(logger, sweeper, managedConfig.Override[bufimagemodify.PhpMetadataNamespaceID]),
		bufimagemodify.RubyPackage(logger, sweeper, managedConfig.Override[bufimagemodify.RubyPackageID]),
	)
	javaPackagePrefix := bufimagemodify.DefaultJavaPackagePrefix
	if managedConfig.JavaPackagePrefix != "" {
		javaPackagePrefix = managedConfig.JavaPackagePrefix
	}
	javaPackageModifier, err := bufimagemodify.JavaPackage(
		logger,
		sweeper,
		javaPackagePrefix,
		managedConfig.Override[bufimagemodify.JavaPackageID],
	)
	if err != nil {
		return nil, err
	}
	modifier = bufimagemodify.Merge(modifier, javaPackageModifier)
	javaMultipleFilesValue := bufimagemodify.DefaultJavaMultipleFilesValue
	if managedConfig.JavaMultipleFiles != nil {
		javaMultipleFilesValue = *managedConfig.JavaMultipleFiles
	}
	javaMultipleFilesModifier, err := bufimagemodify.JavaMultipleFiles(
		logger,
		sweeper,
		javaMultipleFilesValue,
		managedConfig.Override[bufimagemodify.JavaMultipleFilesID],
	)
	if err != nil {
		return nil, err
	}
	modifier = bufimagemodify.Merge(modifier, javaMultipleFilesModifier)
	if managedConfig.CcEnableArenas != nil {
		ccEnableArenasModifier, err := bufimagemodify.CcEnableArenas(
			logger,
			sweeper,
			*managedConfig.CcEnableArenas,
			managedConfig.Override[bufimagemodify.CcEnableArenasID],
		)
		if err != nil {
			return nil, err
		}
		modifier = bufimagemodify.Merge(modifier, ccEnableArenasModifier)
	}
	if managedConfig.JavaStringCheckUtf8 != nil {
		javaStringCheckUtf8, err := bufimagemodify.JavaStringCheckUtf8(
			logger,
			sweeper,
			*managedConfig.JavaStringCheckUtf8,
			managedConfig.Override[bufimagemodify.JavaStringCheckUtf8ID],
		)
		if err != nil {
			return nil, err
		}
		modifier = bufimagemodify.Merge(modifier, javaStringCheckUtf8)
	}
	if managedConfig.OptimizeFor != nil {
		optimizeFor, err := bufimagemodify.OptimizeFor(
			logger,
			sweeper,
			*managedConfig.OptimizeFor,
			managedConfig.Override[bufimagemodify.OptimizeForID],
		)
		if err != nil {
			return nil, err
		}
		modifier = bufimagemodify.Merge(
			modifier,
			optimizeFor,
		)
	}
	if managedConfig.GoPackagePrefixConfig != nil {
		goPackageModifier, err := bufimagemodify.GoPackage(
			logger,
			sweeper,
			managedConfig.GoPackagePrefixConfig.Default,
			managedConfig.GoPackagePrefixConfig.Except,
			managedConfig.GoPackagePrefixConfig.Override,
			managedConfig.Override[bufimagemodify.GoPackageID],
		)
		if err != nil {
			return nil, fmt.Errorf("failed to construct go_package modifier: %w", err)
		}
		modifier = bufimagemodify.Merge(
			modifier,
			goPackageModifier,
		)
	}
	return modifier, nil
}

// validateResponses verifies that a response is set for each of the
// pluginConfigs, and that each generated file is generated by a single
// plugin.
func validateResponses(
	responses []*pluginpb.CodeGeneratorResponse,
	pluginConfigs []*PluginConfig,
) error {
	if len(responses) != len(pluginConfigs) {
		return fmt.Errorf("unexpected number of responses: expected %d but got %d", len(pluginConfigs), len(responses))
	}
	fileToPluginIndex := make(map[string]int)
	for i, response := range responses {
		pluginConfig := pluginConfigs[i]
		if response == nil {
			return fmt.Errorf("failed to create a response for %q", pluginConfig.PluginName())
		}
		index := i
		for _, file := range response.File {
			if file.GetInsertionPoint() != "" {
				// We expect duplicate filenames for insertion points.
				continue
			}
			filename := file.GetName()
			if originalIndex, ok := fileToPluginIndex[filename]; ok {
				originalPluginConfig := pluginConfigs[originalIndex]
				return fmt.Errorf(
					"file %q was generated multiple times: once by plugin %q and again by plugin %q",
					filename,
					originalPluginConfig.PluginName(),
					pluginConfig.PluginName(),
				)
			}
			fileToPluginIndex[filename] = index
		}
	}
	return nil
}

type generateOptions struct {
	baseOutDirPath string
	includeImports bool
}

func newGenerateOptions() *generateOptions {
	return &generateOptions{}
}
