/*
Copyright 2023 fengxsong@outlook.com

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package registry

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	stdsync "sync"
	"time"

	"github.com/containers/common/pkg/auth"
	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/types"
	"golang.org/x/sync/errgroup"

	"github.com/labring/sealos/pkg/constants"
	"github.com/labring/sealos/pkg/registry/handler"
	"github.com/labring/sealos/pkg/registry/sync"
	"github.com/labring/sealos/pkg/ssh"
	v2 "github.com/labring/sealos/pkg/types/v1beta1"
	"github.com/labring/sealos/pkg/utils/file"
	httputils "github.com/labring/sealos/pkg/utils/http"
	"github.com/labring/sealos/pkg/utils/logger"
)

const (
	localhost            = "127.0.0.1"
	defaultTemporaryPort = "5050"
)

// TODO: fallback to ssh mode when HTTP is not available
type syncMode struct {
	pathResolver PathResolver
	ssh          ssh.Interface
	mounts       []v2.MountImage
}

func shouldSkip(mounts []v2.MountImage) bool {
	for i := range mounts {
		if file.IsDir(filepath.Join(mounts[i].MountPoint, constants.RegistryDirName)) {
			return false
		}
	}
	return true
}

func (s *syncMode) Sync(ctx context.Context, hosts ...string) error {
	if shouldSkip(s.mounts) {
		return nil
	}
	sys := &types.SystemContext{
		DockerInsecureSkipTLSVerify: types.OptionalBoolTrue,
	}
	logger.Info("using sync mode syncing images to hosts %v", hosts)
	// run `sealctl registry serve` to start a temporary registry
	for i := range hosts {
		cmdCtx, cancel := context.WithCancel(ctx)
		// defer cancel async commands
		defer cancel()
		go func(ctx context.Context, host string) {
			logger.Debug("running temporary registry on host %s", host)
			if err := s.ssh.CmdAsyncWithContext(ctx, host, getRegistryServeCommand(s.pathResolver, defaultTemporaryPort)); err != nil {
				logger.Error(err)
			}
		}(cmdCtx, hosts[i])
	}

	var endpoints []string
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	eg, _ := errgroup.WithContext(probeCtx)
	mutex := &stdsync.Mutex{}
	for i := range hosts {
		host := hosts[i]
		eg.Go(func() error {
			ep := sync.ParseRegistryAddress(trimPortStr(host), defaultTemporaryPort)
			if err := httputils.WaitUntilEndpointAlive(probeCtx, "http://"+ep); err != nil {
				return err
			}
			mutex.Lock()
			endpoints = append(endpoints, ep)
			mutex.Unlock()
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}

	outerEg, ctx := errgroup.WithContext(ctx)
	for i := range s.mounts {
		registryDir := filepath.Join(s.mounts[i].MountPoint, constants.RegistryDirName)
		if !file.IsDir(registryDir) {
			continue
		}
		outerEg.Go(func() error {
			config, err := handler.NewConfig(registryDir, 0)
			if err != nil {
				return err
			}
			config.Log.AccessLog.Disabled = true
			errCh := handler.Run(ctx, config)

			eg, inner := errgroup.WithContext(ctx)
			for j := range endpoints {
				dst := endpoints[j]
				eg.Go(func() error {
					src := sync.ParseRegistryAddress(localhost, config.HTTP.Addr)
					probeCtx, cancel := context.WithTimeout(inner, time.Second*3)
					defer cancel()
					if err = httputils.WaitUntilEndpointAlive(probeCtx, "http://"+src); err != nil {
						return err
					}
					opts := &sync.Options{
						SystemContext: sys,
						Source:        src,
						Target:        dst,
						SelectionOptions: []copy.ImageListSelection{
							copy.CopyAllImages, copy.CopySystemImage,
						},
						OmitError: true,
					}

					if err = sync.ToRegistry(inner, opts); err == nil {
						return nil
					}
					if !strings.Contains(err.Error(), "manifest unknown") {
						return err
					}
					return nil
				})
			}
			go func() {
				errCh <- eg.Wait()
			}()
			return <-errCh
		})
	}
	return outerEg.Wait()
}

func trimPortStr(s string) string {
	if idx := strings.Index(s, ":"); idx > 0 {
		return s[:idx]
	}
	return s
}

func getRegistryServeCommand(pathResolver PathResolver, port string) string {
	return fmt.Sprintf("%s registry serve filesystem -p %s --disable-logging=true %s",
		pathResolver.RootFSSealctlPath(), port, pathResolver.RootFSRegistryPath(),
	)
}

//lint:ignore U1000 Ignore unused function temporarily for debugging
func loginRegistry(ctx context.Context, sys *types.SystemContext, username, password, registry string) error {
	return auth.Login(ctx, sys, &auth.LoginOptions{
		Username: username,
		Password: password,
		Stdout:   io.Discard,
	}, []string{registry})
}
