// Copyright 2017 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package containerd

import (
	"flag"
	"fmt"
	"net"
	"path"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"k8s.io/klog/v2"

	"github.com/containerd/containerd/pkg/dialer"
	"github.com/google/cadvisor/container"
	criapi "github.com/google/cadvisor/container/cri-api/pkg/apis/runtime/v1alpha2" // TODO: update to v1
	"github.com/google/cadvisor/container/libcontainer"
	"github.com/google/cadvisor/fs"
	info "github.com/google/cadvisor/info/v1"
	"github.com/google/cadvisor/watcher"
)

var ArgContainerdEndpoint = flag.String("containerd", "/run/containerd/containerd.sock", "containerd endpoint")
var ArgContainerdNamespace = flag.String("containerd-namespace", "k8s.io", "containerd namespace")

// The namespace under which containerd aliases are unique.
const k8sContainerdNamespace = "containerd"

// Regexp that identifies containerd cgroups, containers started with
// --cgroup-parent have another prefix than 'containerd'
var containerdCgroupRegexp = regexp.MustCompile(`([a-z0-9]{64})`)

var containerdEnvWhitelist = flag.String("containerd_env_metadata_whitelist", "", "a comma-separated list of environment variable keys matched with specified prefix that needs to be collected for containerd containers")

var criClient criapi.RuntimeServiceClient = nil
var onceCriClient sync.Once

type containerdFactory struct {
	machineInfoFactory info.MachineInfoFactory
	client             criapi.RuntimeServiceClient
	version            string
	// Information about the mounted cgroup subsystems.
	cgroupSubsystems libcontainer.CgroupSubsystems
	// Information about mounted filesystems.
	fsInfo          fs.FsInfo
	includedMetrics container.MetricSet
}

func (f *containerdFactory) String() string {
	return k8sContainerdNamespace
}

func (f *containerdFactory) NewContainerHandler(name string, inHostNamespace bool) (handler container.ContainerHandler, err error) {
	err = NewCriClient()
	if err != nil {
		return
	}

	metadataEnvs := strings.Split(*containerdEnvWhitelist, ",")

	return newContainerdContainerHandler(
		criClient,
		name,
		f.machineInfoFactory,
		f.fsInfo,
		&f.cgroupSubsystems,
		inHostNamespace,
		metadataEnvs,
		f.includedMetrics,
	)
}

// Returns the containerd ID from the full container name.
func ContainerNameToContainerdID(name string) string {
	id := path.Base(name)
	if matches := containerdCgroupRegexp.FindStringSubmatch(id); matches != nil {
		return matches[1]
	}
	return id
}

// isContainerName returns true if the cgroup with associated name
// corresponds to a containerd container.
func isContainerName(name string) bool {
	// TODO: May be check with HasPrefix ContainerdNamespace
	if strings.HasSuffix(name, ".mount") {
		return false
	}
	return containerdCgroupRegexp.MatchString(path.Base(name))
}

// Containerd can handle and accept all containerd created containers
func (f *containerdFactory) CanHandleAndAccept(name string) (bool, bool, error) {
	// if the container is not associated with containerd, we can't handle it or accept it.
	if !isContainerName(name) {
		return false, false, nil
	}
	// Check if the container is known to containerd and it is running.
	id := ContainerNameToContainerdID(name)
	// If container and task lookup in containerd fails then we assume
	// that the container state is not known to containerd
	ctx := context.Background()
	_, err := f.client.ContainerStatus(ctx, &criapi.ContainerStatusRequest{
		ContainerId: id,
	})
	if err != nil {
		return false, false, fmt.Errorf("failed to load container: %v", err)
	}

	return true, true, nil
}

func (f *containerdFactory) DebugInfo() map[string][]string {
	return map[string][]string{}
}

// Register root container before running this function!
func Register(factory info.MachineInfoFactory, fsInfo fs.FsInfo, includedMetrics container.MetricSet) error {
	err := NewCriClient()
	if err != nil {
		return fmt.Errorf("unable to create containerd client: %v", err)
	}

	versionRes, err := criClient.Version(context.Background(), &criapi.VersionRequest{})
	if err != nil {
		return fmt.Errorf("failed to fetch containerd client version: %v", err)
	}

	cgroupSubsystems, err := libcontainer.GetCgroupSubsystems(includedMetrics)
	if err != nil {
		return fmt.Errorf("failed to get cgroup subsystems: %v", err)
	}

	klog.V(1).Infof("Registering containerd factory")
	f := &containerdFactory{
		cgroupSubsystems:   cgroupSubsystems,
		client:             criClient,
		fsInfo:             fsInfo,
		machineInfoFactory: factory,
		version:            versionRes.RuntimeVersion,
		includedMetrics:    includedMetrics,
	}

	container.RegisterContainerHandlerFactory(f, []watcher.ContainerWatchSource{watcher.Raw})
	return nil
}

func NewCriClient() error {
	var retErr error
	onceCriClient.Do(func() {
		tryConn, err := net.DialTimeout("unix", *ArgContainerdEndpoint, connectionTimeout)
		if err != nil {
			retErr = fmt.Errorf("containerd: cannot unix dial containerd cri service: %v", err)
			return
		}
		tryConn.Close()

		connParams := grpc.ConnectParams{
			Backoff: backoff.DefaultConfig,
		}
		connParams.Backoff.BaseDelay = baseBackoffDelay
		connParams.Backoff.MaxDelay = maxBackoffDelay
		gopts := []grpc.DialOption{
			grpc.WithInsecure(),
			grpc.WithContextDialer(dialer.ContextDialer),
			grpc.WithBlock(),
			grpc.WithConnectParams(connParams),
		}
		unary, stream := newNSInterceptors(*ArgContainerdNamespace)
		gopts = append(gopts,
			grpc.WithUnaryInterceptor(unary),
			grpc.WithStreamInterceptor(stream),
		)

		ctx, cancel := context.WithTimeout(context.Background(), connectionTimeout)
		defer cancel()
		conn, err := grpc.DialContext(ctx, dialer.DialAddress(*ArgContainerdEndpoint), gopts...)
		if err != nil {
			retErr = err
			return
		}
		criClient = criapi.NewRuntimeServiceClient(conn)
	})
	return retErr
}
