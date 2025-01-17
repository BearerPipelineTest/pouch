package v1alpha2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"time"

	"github.com/alibaba/pouch/apis/filters"
	apitypes "github.com/alibaba/pouch/apis/types"
	anno "github.com/alibaba/pouch/cri/annotations"
	runtime "github.com/alibaba/pouch/cri/apis/v1alpha2"
	"github.com/alibaba/pouch/cri/metrics"
	cni "github.com/alibaba/pouch/cri/ocicni"
	"github.com/alibaba/pouch/cri/stream"
	metatypes "github.com/alibaba/pouch/cri/v1alpha2/types"
	"github.com/alibaba/pouch/ctrd"
	"github.com/alibaba/pouch/daemon/config"
	"github.com/alibaba/pouch/daemon/mgr"
	"github.com/alibaba/pouch/hookplugins"
	"github.com/alibaba/pouch/pkg/errtypes"
	"github.com/alibaba/pouch/pkg/log"
	"github.com/alibaba/pouch/pkg/meta"
	"github.com/alibaba/pouch/pkg/reference"
	pkgstreams "github.com/alibaba/pouch/pkg/streams"
	"github.com/alibaba/pouch/pkg/utils"
	util_metrics "github.com/alibaba/pouch/pkg/utils/metrics"
	"github.com/alibaba/pouch/version"

	"github.com/pkg/errors"
)

const (
	pouchRuntimeName         = "pouch"
	kubeletRuntimeAPIVersion = "0.1.0"

	// kubePrefix is used to identify the containers/sandboxes on the node managed by kubelet.
	kubePrefix = "k8s"

	// annotationPrefix is used to distinguish between annotations and labels.
	annotationPrefix = "annotation."

	// Internal pouch labels used to identify whether a container is a sandbox
	// or a regular container.
	containerTypeLabelKey       = "io.kubernetes.pouch.type"
	containerTypeLabelSandbox   = "sandbox"
	containerTypeLabelContainer = "container"
	sandboxIDLabelKey           = "io.kubernetes.sandbox.id"
	containerLogPathLabelKey    = "io.kubernetes.container.logpath"

	// sandboxContainerName is a string to include in the pouch container so
	// that users can easily identify the sandboxes.
	sandboxContainerName = "POD"

	// nameDelimiter is used to construct pouch container names.
	nameDelimiter = "_"

	namespaceModeHost = "host"

	// resolvConfPath is the abs path of resolv.conf on host or container.
	resolvConfPath = "/etc/resolv.conf"

	// snapshotPlugin implements a snapshotter.
	snapshotPlugin = "io.containerd.snapshotter.v1"

	// networkNotReadyReason is the reason reported when network is not ready.
	networkNotReadyReason = "NetworkPluginNotReady"
)

var (
	// Default timeout for stopping container.
	defaultStopTimeout = int64(10)
)

// CriMgr as an interface defines all operations against CRI.
type CriMgr interface {
	// RuntimeServiceServer is interface of CRI runtime service.
	runtime.RuntimeServiceServer

	// ImageServiceServer is interface of CRI image service.
	runtime.ImageServiceServer

	// VolumeServiceServer is interface of CRI volume service.
	runtime.VolumeServiceServer

	// StreamServerStart starts the stream server of CRI.
	StreamServerStart() error

	// StreamStart returns the router of Stream Server.
	StreamRouter() stream.Router
}

// CriManager is an implementation of interface CriMgr.
type CriManager struct {
	ContainerMgr mgr.ContainerMgr
	ImageMgr     mgr.ImageMgr
	VolumeMgr    mgr.VolumeMgr
	CniMgr       cni.CniMgr
	CriPlugin    hookplugins.CriPlugin

	// StreamServer is the stream server of CRI serves container streaming request.
	StreamServer StreamServer

	// SandboxBaseDir is the directory used to store sandbox files like /etc/hosts, /etc/resolv.conf, etc.
	SandboxBaseDir string

	// SandboxImage is the image used by sandbox container.
	SandboxImage string

	// SandboxStore stores the configuration of sandboxes.
	SandboxStore *meta.Store

	// SnapshotStore stores information of all snapshots.
	SnapshotStore *mgr.SnapshotStore

	// imageFSPath is the path to image filesystem.
	imageFSPath string

	// DaemonConfig is the config of daemon
	DaemonConfig *config.Config
}

// NewCriManager creates a brand new cri manager.
func NewCriManager(config *config.Config, ctrMgr mgr.ContainerMgr, imgMgr mgr.ImageMgr, volumeMgr mgr.VolumeMgr, criPlugin hookplugins.CriPlugin) (CriMgr, error) {
	streamCfg, err := toStreamConfig(config)
	if err != nil {
		return nil, err
	}
	streamServer, err := NewStreamServer(streamCfg, stream.NewStreamRuntime(ctrMgr))
	if err != nil {
		return nil, fmt.Errorf("failed to create stream server for cri manager: %v", err)
	}

	c := &CriManager{
		ContainerMgr:   ctrMgr,
		ImageMgr:       imgMgr,
		VolumeMgr:      volumeMgr,
		CriPlugin:      criPlugin,
		StreamServer:   streamServer,
		SandboxBaseDir: path.Join(config.HomeDir, "sandboxes"),
		SandboxImage:   config.CriConfig.SandboxImage,
		SnapshotStore:  mgr.NewSnapshotStore(),
		DaemonConfig:   config,
	}
	c.CniMgr, err = cni.NewCniManager(&config.CriConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create cni manager: %v", err)
	}

	c.SandboxStore, err = meta.NewStore(meta.Config{
		Driver:  "local",
		BaseDir: path.Join(config.HomeDir, "sandboxes-meta"),
		Buckets: []meta.Bucket{
			{
				Name: meta.MetaJSONFile,
				Type: reflect.TypeOf(metatypes.SandboxMeta{}),
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create sandbox meta store: %v", err)
	}

	c.imageFSPath = imageFSPath(path.Join(config.HomeDir, "containerd/root"), ctrd.CurrentSnapshotterName(context.TODO()))
	log.With(nil).Infof("Get image filesystem path %q", c.imageFSPath)

	if config.CriConfig.EnableCriStatsCollect {
		period := config.CriConfig.CriStatsCollectPeriod
		if period <= 0 {
			return nil, fmt.Errorf("cri stats collect period should > 0")
		}
		snapshotsSyncer := ctrMgr.NewSnapshotsSyncer(
			c.SnapshotStore,
			time.Duration(period)*time.Second,
		)
		snapshotsSyncer.Start()
	} else {
		log.With(nil).Infof("disable cri to collect stats from containerd periodically")
	}

	return c, nil
}

// StreamServerStart starts the stream server of CRI.
func (c *CriManager) StreamServerStart() error {
	return c.StreamServer.Start()
}

// StreamRouter returns the router of Stream StreamServer.
func (c *CriManager) StreamRouter() stream.Router {
	return c.StreamServer
}

// TODO: Move the underlying functions to their respective files in the future.

// Version returns the runtime name, runtime version and runtime API version.
func (c *CriManager) Version(ctx context.Context, r *runtime.VersionRequest) (*runtime.VersionResponse, error) {
	return &runtime.VersionResponse{
		Version:           kubeletRuntimeAPIVersion,
		RuntimeName:       pouchRuntimeName,
		RuntimeVersion:    version.Version,
		RuntimeApiVersion: version.APIVersion,
	}, nil
}

// RunPodSandbox creates and starts a pod-level sandbox. Runtimes should ensure
// the sandbox is in ready state.
func (c *CriManager) RunPodSandbox(ctx context.Context, r *runtime.RunPodSandboxRequest) (_ *runtime.RunPodSandboxResponse, retErr error) {
	label := util_metrics.ActionRunLabel
	defer func(start time.Time) {
		metrics.PodActionsCounter.WithLabelValues(label).Inc()
		metrics.PodActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	config := r.GetConfig()

	if config.GetMetadata() == nil {
		return nil, fmt.Errorf("sandbox metadata required")
	}

	// Step 1: Prepare image for the sandbox.
	image := c.SandboxImage

	// Make sure the sandbox image exists.
	err := c.ensureSandboxImageExists(ctx, image)
	if err != nil {
		return nil, err
	}

	// prepare the sandboxID and store it.
	id, err := c.generateSandboxID(ctx)
	if err != nil {
		return nil, err
	}
	sandboxMeta := &metatypes.SandboxMeta{
		ID: id,
	}
	if err := c.SandboxStore.Put(sandboxMeta); err != nil {
		return nil, err
	}

	// If running sandbox failed, clean up the sandboxMeta from sandboxStore.
	// We should clean it until the container has been removed successfully by Pouchd.
	removeContainerErr := false
	defer func() {
		if retErr != nil && !removeContainerErr {
			if err := c.SandboxStore.Remove(id); err != nil {
				log.With(ctx).Errorf("failed to remove the metadata of container %q from sandboxStore: %v", id, err)
			}
		}
	}()

	// Step 2: Setup networking for the sandbox.

	// If it is in host network, no need to configure the network of sandbox.
	if sandboxNetworkMode(config) != runtime.NamespaceMode_NODE {
		sandboxMeta.NetNS, err = c.CniMgr.NewNetNS()
		if err != nil {
			return nil, err
		}
		defer func() {
			if retErr != nil {
				if err := c.CniMgr.RemoveNetNS(sandboxMeta.NetNS); err != nil {
					log.With(ctx).Errorf("failed to remove net ns for sandbox %q: %v", id, err)
				}
			}
		}()
		if err := c.setupPodNetwork(id, sandboxMeta.NetNS, config); err != nil {
			return nil, err
		}
		defer func() {
			if retErr != nil {
				if err := c.teardownNetwork(id, sandboxMeta.NetNS, config); err != nil {
					log.With(ctx).Errorf("failed to teardown pod network for sandbox %q: %v", id, err)
				}
			}
		}()
	}

	// Step 3: Create the sandbox container.

	// applies the runtime of container specified by the caller.
	if err := c.applySandboxRuntimeHandler(sandboxMeta, r.GetRuntimeHandler(), config.GetAnnotations()); err != nil {
		return nil, err
	}

	// applies the annotations extended.
	if err := c.applySandboxAnnotations(sandboxMeta, config.GetAnnotations()); err != nil {
		return nil, err
	}

	createConfig, err := makeSandboxPouchConfig(config, sandboxMeta, image)

	if err != nil {
		return nil, fmt.Errorf("failed to make sandbox pouch config for pod %q: %v", config.GetMetadata().GetName(), err)
	}
	createConfig.SpecificID = id

	sandboxName := makeSandboxName(config)

	_, err = c.ContainerMgr.Create(ctx, sandboxName, createConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create a sandbox for pod %q: %v", config.Metadata.Name, err)
	}

	sandboxMeta.Config = config
	if err := c.SandboxStore.Put(sandboxMeta); err != nil {
		return nil, err
	}

	// If running sandbox failed, clean up the container.
	defer func() {
		if retErr != nil {
			if err := c.ContainerMgr.Remove(ctx, id, &apitypes.ContainerRemoveOptions{Volumes: true, Force: true}); err != nil {
				removeContainerErr = true
				log.With(ctx).Errorf("failed to remove container when running sandbox failed %q: %v", id, err)
			}
		}
	}()

	// Step 4: Start the sandbox container.
	err = c.ContainerMgr.Start(ctx, id, &apitypes.ContainerStartOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to start sandbox container for pod %q: %v", config.GetMetadata().GetName(), err)
	}

	sandboxRootDir := path.Join(c.SandboxBaseDir, id)
	err = os.MkdirAll(sandboxRootDir, 0755)
	if err != nil {
		return nil, fmt.Errorf("failed to create sandbox root directory: %v", err)
	}
	defer func() {
		// If running sandbox failed, clean up the sandbox directory.
		if retErr != nil {
			if err := os.RemoveAll(sandboxRootDir); err != nil {
				log.With(ctx).Errorf("failed to clean up the directory of sandbox %q: %v", id, err)
			}
		}
	}()

	// Setup sandbox file /etc/resolv.conf.
	err = setupSandboxFiles(sandboxRootDir, config)
	if err != nil {
		return nil, fmt.Errorf("failed to setup sandbox files: %v", err)
	}

	metrics.PodSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.RunPodSandboxResponse{PodSandboxId: id}, nil
}

// StartPodSandbox restart a sandbox pod which was stopped by accident
// and we should reconfigure it with network plugin which will make sure it reacquire its original network configuration,
// like IP address.
func (c *CriManager) StartPodSandbox(ctx context.Context, r *runtime.StartPodSandboxRequest) (_ *runtime.StartPodSandboxResponse, retErr error) {
	label := util_metrics.ActionStartLabel
	defer func(start time.Time) {
		metrics.PodActionsCounter.WithLabelValues(label).Inc()
		metrics.PodActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	podSandboxID := r.GetPodSandboxId()

	sandbox, err := c.ContainerMgr.Get(ctx, podSandboxID)
	if err != nil {
		return nil, fmt.Errorf("failed to get container %q: %v", podSandboxID, err)
	}

	res, err := c.SandboxStore.Get(podSandboxID)
	if err != nil {
		return nil, fmt.Errorf("failed to get metadata of %q from SandboxStore: %v", podSandboxID, err)
	}
	sandboxMeta := res.(*metatypes.SandboxMeta)

	if mgr.IsNetNS(sandbox.HostConfig.NetworkMode) {
		ip, _ := c.CniMgr.GetPodNetworkStatus(sandboxMeta.NetNS)
		// recover network if it is down.
		if ip == "" {
			if err := c.CniMgr.RecoverNetNS(sandboxMeta.NetNS); err != nil {
				return nil, fmt.Errorf("failed to recover netns %s for sandbox %q: %v", sandboxMeta.NetNS, podSandboxID, err)
			}
			defer func() {
				if retErr != nil {
					if err := c.CniMgr.RemoveNetNS(sandboxMeta.NetNS); err != nil {
						log.With(ctx).Errorf("failed to remove net ns for sandbox %q: %v", podSandboxID, err)
					}
				}
			}()

			if err = c.setupPodNetwork(podSandboxID, sandboxMeta.NetNS, sandboxMeta.Config); err != nil {
				return nil, err
			}
			defer func() {
				if retErr != nil {
					if err := c.teardownNetwork(podSandboxID, sandboxMeta.NetNS, sandboxMeta.Config); err != nil {
						log.With(ctx).Errorf("failed to teardown pod network for sandbox %q: %v", podSandboxID, err)
					}
				}
			}()
		}
	}

	// start PodSandbox.
	startErr := c.ContainerMgr.Start(ctx, podSandboxID, &apitypes.ContainerStartOptions{})
	if startErr != nil {
		return nil, fmt.Errorf("failed to start podSandbox %q: %v", podSandboxID, startErr)
	}
	defer func() {
		if retErr != nil {
			stopErr := c.ContainerMgr.Stop(ctx, podSandboxID, defaultStopTimeout)
			if stopErr != nil {
				log.With(ctx).Errorf("failed to stop sandbox %q: %v", podSandboxID, stopErr)
			}
		}
	}()

	// legacy container using /proc/$pid/ns/net as the sandbox netns.
	if mgr.IsNone(sandbox.HostConfig.NetworkMode) {
		if err = c.setupPodNetwork(podSandboxID, containerNetns(sandbox), sandboxMeta.Config); err != nil {
			return nil, err
		}
	}

	// Setup sandbox file /etc/resolv.conf again to ensure resolv.conf is right
	sandboxRootDir := path.Join(c.SandboxBaseDir, sandbox.ID)
	err = setupSandboxFiles(sandboxRootDir, sandboxMeta.Config)
	if err != nil {
		return nil, fmt.Errorf("failed to setup sandbox files: %v", err)
	}

	metrics.PodSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.StartPodSandboxResponse{}, nil
}

// StopPodSandbox stops the sandbox. If there are any running containers in the
// sandbox, they should be forcibly terminated.
// notes:
// 1. for legacy dockershim style container, lifecycle of podNetwork is bound to container
// using /proc/$pid/ns/net. When stopping sandbox, we first teardown the pod network, then stop
// the sandbox container.
// 2. In newly implementation. We first create an empty netns and setup pod network inside it,
// which is independent from container lifecycle. When stopping sandbox, we first stop container,
// then teardown the pod network, which is a reverse operation of RunPodSandbox.
func (c *CriManager) StopPodSandbox(ctx context.Context, r *runtime.StopPodSandboxRequest) (*runtime.StopPodSandboxResponse, error) {
	label := util_metrics.ActionStopLabel
	defer func(start time.Time) {
		metrics.PodActionsCounter.WithLabelValues(label).Inc()
		metrics.PodActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	podSandboxID := r.GetPodSandboxId()
	res, err := c.SandboxStore.Get(podSandboxID)
	if err != nil {
		return nil, fmt.Errorf("failed to get metadata of %q from SandboxStore: %v", podSandboxID, err)
	}
	sandboxMeta := res.(*metatypes.SandboxMeta)

	opts := &mgr.ContainerListOption{All: true}
	filter := func(c *mgr.Container) bool {
		return c.Config.Labels[sandboxIDLabelKey] == podSandboxID
	}
	opts.FilterFunc = filter

	containers, err := c.ContainerMgr.List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to get the containers belong to sandbox %q: %v", podSandboxID, err)
	}

	// Stop all containers in the sandbox.
	for _, container := range containers {
		err = c.ContainerMgr.Stop(ctx, container.ID, defaultStopTimeout)
		if err != nil {
			if errtypes.IsNotfound(err) {
				log.With(ctx).Warningf("container %q of sandbox %q not found", container.ID, podSandboxID)
				continue
			}
			return nil, fmt.Errorf("failed to stop container %q of sandbox %q: %v", container.ID, podSandboxID, err)
		}
		log.With(ctx).Infof("success to stop container %q of sandbox %q", container.ID, podSandboxID)
	}

	// Teardown network of the legacy dockershim style pod, if it is not in host network mode.
	if sandboxNetworkMode(sandboxMeta.Config) != runtime.NamespaceMode_NODE && sandboxMeta.NetNS == "" {
		container, err := c.ContainerMgr.Get(ctx, podSandboxID)
		if err != nil {
			return nil, err
		}
		if err = c.teardownNetwork(podSandboxID, containerNetns(container), sandboxMeta.Config); err != nil {
			return nil, fmt.Errorf("failed to teardown network of sandbox %s, ns path %s: %v", podSandboxID, sandboxMeta.NetNS, err)
		}
	}

	// Stop the sandbox container.
	err = c.ContainerMgr.Stop(ctx, podSandboxID, defaultStopTimeout)
	// if the sandbox container has been removed by 'pouch rm', treat this situation as success
	// in order to teardown the network.
	if err != nil {
		if errtypes.IsNotfound(err) {
			log.With(ctx).Warningf("sandbox container %q not found", podSandboxID)
		} else {
			return nil, fmt.Errorf("failed to stop sandbox %q: %v", podSandboxID, err)
		}
	}

	// After container stop, no one refer the net namespace, do the clean up job.
	if sandboxNetworkMode(sandboxMeta.Config) != runtime.NamespaceMode_NODE && sandboxMeta.NetNS != "" {
		if err := c.teardownNetwork(podSandboxID, sandboxMeta.NetNS, sandboxMeta.Config); err != nil {
			return nil, fmt.Errorf("failed to teardown network of sandbox %s, ns path %s: %v", podSandboxID, sandboxMeta.NetNS, err)
		}
		if err := c.CniMgr.CloseNetNS(sandboxMeta.NetNS); err != nil {
			return nil, fmt.Errorf("failed to close net ns %s of sandbox %q: %v", sandboxMeta.NetNS, podSandboxID, err)
		}
		if err := c.CniMgr.RemoveNetNS(sandboxMeta.NetNS); err != nil {
			return nil, fmt.Errorf("failed to remove net ns %s of sandbox %q: %v", sandboxMeta.NetNS, podSandboxID, err)
		}
	}

	metrics.PodSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.StopPodSandboxResponse{}, nil
}

// RemovePodSandbox removes the sandbox. If there are running containers in the
// sandbox, they should be forcibly removed.
func (c *CriManager) RemovePodSandbox(ctx context.Context, r *runtime.RemovePodSandboxRequest) (*runtime.RemovePodSandboxResponse, error) {
	label := util_metrics.ActionRemoveLabel
	defer func(start time.Time) {
		metrics.PodActionsCounter.WithLabelValues(label).Inc()
		metrics.PodActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	podSandboxID := r.GetPodSandboxId()

	opts := &mgr.ContainerListOption{All: true}
	filter := func(c *mgr.Container) bool {
		return c.Config.Labels[sandboxIDLabelKey] == podSandboxID
	}
	opts.FilterFunc = filter

	containers, err := c.ContainerMgr.List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to remove sandbox %q: %v", podSandboxID, err)
	}

	// Remove all containers in the sandbox.
	for _, container := range containers {
		if err := c.ContainerMgr.Remove(ctx, container.ID, &apitypes.ContainerRemoveOptions{Volumes: true, Force: true}); err != nil {
			if errtypes.IsNotfound(err) {
				log.With(ctx).Warningf("container %q of sandbox %q not found", container.ID, podSandboxID)
				continue
			}
			return nil, fmt.Errorf("failed to remove container %q of sandbox %q: %v", container.ID, podSandboxID, err)
		}

		log.With(ctx).Infof("success to remove container %q of sandbox %q", container.ID, podSandboxID)
	}

	// Remove the sandbox container.
	if err := c.ContainerMgr.Remove(ctx, podSandboxID, &apitypes.ContainerRemoveOptions{Volumes: true, Force: true}); err != nil {
		if errtypes.IsNotfound(err) {
			log.With(ctx).Warningf("sandbox container %q not found", podSandboxID)
		} else {
			return nil, fmt.Errorf("failed to remove sandbox %q: %v", podSandboxID, err)
		}
	}

	// Cleanup the sandbox root directory.
	sandboxRootDir := path.Join(c.SandboxBaseDir, podSandboxID)

	if err := os.RemoveAll(sandboxRootDir); err != nil {
		return nil, fmt.Errorf("failed to remove root directory %q: %v", sandboxRootDir, err)
	}

	if err := c.SandboxStore.Remove(podSandboxID); err != nil {
		return nil, fmt.Errorf("failed to remove meta %q: %v", sandboxRootDir, err)
	}

	metrics.PodSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.RemovePodSandboxResponse{}, nil
}

// PodSandboxStatus returns the status of the PodSandbox.
func (c *CriManager) PodSandboxStatus(ctx context.Context, r *runtime.PodSandboxStatusRequest) (*runtime.PodSandboxStatusResponse, error) {
	label := util_metrics.ActionStatusLabel
	defer func(start time.Time) {
		metrics.PodActionsCounter.WithLabelValues(label).Inc()
		metrics.PodActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	podSandboxID := r.GetPodSandboxId()

	res, err := c.SandboxStore.Get(podSandboxID)
	if err != nil {
		return nil, fmt.Errorf("failed to get metadata of %q from SandboxStore: %v", podSandboxID, err)
	}
	sandboxMeta := res.(*metatypes.SandboxMeta)

	// partially created sandbox.
	// kubelet won't call this method because the partially created sandbox
	// are removed from ListPodSandbox interface.
	if sandboxMeta.Config == nil {
		return nil, fmt.Errorf("failed to get status of partially sandbox %q: %v", podSandboxID, err)
	}

	sandbox, err := c.ContainerMgr.Get(ctx, podSandboxID)
	if err != nil {
		if errtypes.IsNotfound(err) {
			return &runtime.PodSandboxStatusResponse{
				Status: &runtime.PodSandboxStatus{
					Id:        podSandboxID,
					State:     runtime.PodSandboxState_SANDBOX_NOTFOUND,
					Metadata:  sandboxMeta.Config.Metadata,
					CreatedAt: 1,
				},
			}, nil
		}
		return nil, fmt.Errorf("failed to get status of sandbox %q: %v", podSandboxID, err)
	}

	// Parse the timestamps.
	createdAt, err := toCriTimestamp(sandbox.Created)
	if err != nil {
		return nil, fmt.Errorf("failed to parse timestamp for sandbox %q: %v", podSandboxID, err)
	}

	// Translate container to sandbox state.
	state := runtime.PodSandboxState_SANDBOX_NOTREADY
	if sandbox.State.Status == apitypes.StatusRunning {
		state = runtime.PodSandboxState_SANDBOX_READY
	}

	labels, annotations := extractLabels(sandbox.Config.Labels)

	nsOpts := sandboxMeta.Config.GetLinux().GetSecurityContext().GetNamespaceOptions()
	hostNet := nsOpts.GetNetwork() == runtime.NamespaceMode_NODE

	var ip string
	// No need to get ip for host network mode.
	if !hostNet {
		ip, err = c.CniMgr.GetPodNetworkStatus(containerNetns(sandbox))
		if err != nil {
			// Maybe the pod has been stopped.
			log.With(ctx).Warnf("failed to get ip of sandbox %q: %v", podSandboxID, err)
		}
	}

	if v, exist := annotations[anno.PassthruKey]; exist && v == "true" {
		ip = annotations[anno.PassthruIP]
	}

	status := &runtime.PodSandboxStatus{
		Id:          podSandboxID,
		State:       state,
		CreatedAt:   createdAt,
		Metadata:    sandboxMeta.Config.Metadata,
		Labels:      labels,
		Annotations: annotations,
		Network:     &runtime.PodSandboxNetworkStatus{Ip: ip},
		Linux: &runtime.LinuxPodSandboxStatus{
			Namespaces: &runtime.Namespace{
				Options: &runtime.NamespaceOption{
					Network: nsOpts.GetNetwork(),
					Pid:     nsOpts.GetPid(),
					Ipc:     nsOpts.GetIpc(),
				},
			},
		},
	}

	metrics.PodSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.PodSandboxStatusResponse{Status: status}, nil
}

// ListPodSandbox returns a list of Sandbox.
func (c *CriManager) ListPodSandbox(ctx context.Context, r *runtime.ListPodSandboxRequest) (*runtime.ListPodSandboxResponse, error) {
	label := util_metrics.ActionListLabel
	defer func(start time.Time) {
		metrics.PodActionsCounter.WithLabelValues(label).Inc()
		metrics.PodActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	sandboxMap, err := c.SandboxStore.List()
	if err != nil {
		return nil, fmt.Errorf("failed to list sandbox from SandboxStore: %v", err)
	}

	sandboxes := make([]*runtime.PodSandbox, 0, len(sandboxMap))
	for id, metadata := range sandboxMap {
		s, err := c.ContainerMgr.Get(ctx, id)
		// metadata exists but container not found
		if err != nil {
			sm, ok := metadata.(*metatypes.SandboxMeta)
			if !ok || sm == nil || sm.Config == nil {
				// partially created sandbox.
				continue
			}

			sandboxes = append(sandboxes, &runtime.PodSandbox{
				Id:          id,
				Metadata:    sm.Config.Metadata,
				State:       runtime.PodSandboxState_SANDBOX_NOTFOUND,
				Labels:      sm.Config.Labels,
				Annotations: sm.Config.Annotations,
				CreatedAt:   1,
			})
			continue
		}
		sandbox, err := toCriSandbox(s)
		if err != nil {
			log.With(ctx).Warningf("failed to parse state of sandbox %q: %v", id, err)
			continue
		}
		sandboxes = append(sandboxes, sandbox)
	}

	result := filterCRISandboxes(sandboxes, r.GetFilter())

	metrics.PodSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.ListPodSandboxResponse{Items: result}, nil
}

// CreateContainer creates a new container in the given PodSandbox.
func (c *CriManager) CreateContainer(ctx context.Context, r *runtime.CreateContainerRequest) (*runtime.CreateContainerResponse, error) {
	label := util_metrics.ActionCreateLabel
	defer func(start time.Time) {
		metrics.ContainerActionsCounter.WithLabelValues(label).Inc()
		metrics.ContainerActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	config := r.GetConfig()
	if config.GetMetadata() == nil {
		return nil, fmt.Errorf("container metadata required")
	}

	sandboxConfig := r.GetSandboxConfig()
	podSandboxID := r.GetPodSandboxId()

	// get sandbox
	sandbox, err := c.ContainerMgr.Get(ctx, podSandboxID)
	if err != nil {
		return nil, fmt.Errorf("failed to get sandbox %q: %v", podSandboxID, err)
	}

	res, err := c.SandboxStore.Get(podSandboxID)
	if err != nil {
		return nil, fmt.Errorf("failed to get metadata of %q from SandboxStore: %v", podSandboxID, err)
	}
	sandboxMeta := res.(*metatypes.SandboxMeta)
	sandboxMeta.NetNS = containerNetns(sandbox)

	labels := makeLabels(config.GetLabels(), config.GetAnnotations())
	// Apply the container type label.
	labels[containerTypeLabelKey] = containerTypeLabelContainer
	// Write the sandbox ID in the labels.
	labels[sandboxIDLabelKey] = podSandboxID
	// Get container log.
	var logPath string
	if config.GetLogPath() != "" {
		logPath = filepath.Join(sandboxConfig.GetLogDirectory(), config.GetLogPath())
		labels[containerLogPathLabelKey] = logPath
	}

	// compatible with both kubernetes and cri-o annotations
	specAnnotation := make(map[string]string)
	specAnnotation[anno.CRIOContainerType] = anno.ContainerTypeContainer
	specAnnotation[anno.ContainerType] = anno.ContainerTypeContainer
	specAnnotation[anno.CRIOSandboxName] = podSandboxID
	specAnnotation[anno.CRIOSandboxID] = podSandboxID
	specAnnotation[anno.SandboxID] = podSandboxID

	resources := r.GetConfig().GetLinux().GetResources()
	createConfig := &apitypes.ContainerCreateConfig{
		ContainerConfig: apitypes.ContainerConfig{
			Entrypoint: config.GetCommand(),
			Cmd:        config.GetArgs(),
			Env:        generateEnvList(config.GetEnvs()),
			Image:      config.GetImage().GetImage(),
			WorkingDir: config.GetWorkingDir(),
			Labels:     labels,
			// Interactive containers:
			OpenStdin:      config.GetStdin(),
			StdinOnce:      config.GetStdinOnce(),
			Tty:            config.GetTty(),
			SpecAnnotation: specAnnotation,
			NetPriority:    config.GetNetPriority(),
			DiskQuota:      resources.GetDiskQuota(),
			QuotaID:        config.GetQuotaId(),
		},
		HostConfig: &apitypes.HostConfig{
			Binds:     generateMountBindings(config.GetMounts()),
			Resources: parseResourcesFromCRI(resources),
		},
		NetworkingConfig: &apitypes.NetworkingConfig{},
	}

	err = c.updateCreateConfig(createConfig, config, sandboxConfig, sandboxMeta)
	if err != nil {
		return nil, err
	}

	// Bindings to overwrite the container's /etc/resolv.conf, /etc/hosts etc.
	sandboxRootDir := path.Join(c.SandboxBaseDir, podSandboxID)
	createConfig.HostConfig.Binds = append(createConfig.HostConfig.Binds, generateContainerMounts(sandboxRootDir)...)

	var devices []*apitypes.DeviceMapping
	for _, device := range config.GetDevices() {
		devices = append(devices, &apitypes.DeviceMapping{
			PathOnHost:        device.GetHostPath(),
			PathInContainer:   device.GetContainerPath(),
			CgroupPermissions: device.GetPermissions(),
		})
	}
	createConfig.HostConfig.Resources.Devices = devices

	containerName := makeContainerName(sandboxConfig, config)

	// call cri plugin to update create config
	if c.CriPlugin != nil {
		if err := c.CriPlugin.PreCreateContainer(ctx, createConfig, sandboxMeta); err != nil {
			return nil, err
		}
	}

	createResp, err := c.ContainerMgr.Create(ctx, containerName, createConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create container for sandbox %q: %v", podSandboxID, err)
	}

	containerID := createResp.ID

	defer func() {
		// If the container failed to be created, clean up the container.
		if err != nil {
			removeErr := c.ContainerMgr.Remove(ctx, containerID, &apitypes.ContainerRemoveOptions{Volumes: true, Force: true})
			if removeErr != nil {
				log.With(ctx).Errorf("failed to remove the container when creating container failed: %v", removeErr)
			}
		}
	}()

	if logPath != "" {
		if err := c.ContainerMgr.AttachCRILog(ctx, containerID, logPath); err != nil {
			return nil, err
		}
	}

	metrics.ContainerSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.CreateContainerResponse{ContainerId: containerID}, nil
}

// StartContainer starts the container.
func (c *CriManager) StartContainer(ctx context.Context, r *runtime.StartContainerRequest) (*runtime.StartContainerResponse, error) {
	label := util_metrics.ActionStartLabel
	defer func(start time.Time) {
		metrics.ContainerActionsCounter.WithLabelValues(label).Inc()
		metrics.ContainerActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	containerID := r.GetContainerId()

	err := c.ContainerMgr.Start(ctx, containerID, &apitypes.ContainerStartOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to start container %q: %v", containerID, err)
	}

	metrics.ContainerSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.StartContainerResponse{}, nil
}

// StopContainer stops a running container with a grace period (i.e., timeout).
func (c *CriManager) StopContainer(ctx context.Context, r *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error) {
	label := util_metrics.ActionStopLabel
	defer func(start time.Time) {
		metrics.ContainerActionsCounter.WithLabelValues(label).Inc()
		metrics.ContainerActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	containerID := r.GetContainerId()

	err := c.ContainerMgr.Stop(ctx, containerID, r.GetTimeout())
	if err != nil {
		return nil, fmt.Errorf("failed to stop container %q: %v", containerID, err)
	}

	metrics.ContainerSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.StopContainerResponse{}, nil
}

// RemoveContainer removes the container.
func (c *CriManager) RemoveContainer(ctx context.Context, r *runtime.RemoveContainerRequest) (*runtime.RemoveContainerResponse, error) {
	label := util_metrics.ActionRemoveLabel
	defer func(start time.Time) {
		metrics.ContainerActionsCounter.WithLabelValues(label).Inc()
		metrics.ContainerActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	containerID := r.GetContainerId()

	if err := c.ContainerMgr.Remove(ctx, containerID, &apitypes.ContainerRemoveOptions{Volumes: true, Force: true}); err != nil {
		return nil, fmt.Errorf("failed to remove container %q: %v", containerID, err)
	}

	metrics.ContainerSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.RemoveContainerResponse{}, nil
}

// ListContainers lists all containers matching the filter.
func (c *CriManager) ListContainers(ctx context.Context, r *runtime.ListContainersRequest) (*runtime.ListContainersResponse, error) {
	label := util_metrics.ActionListLabel
	defer func(start time.Time) {
		metrics.ContainerActionsCounter.WithLabelValues(label).Inc()
		metrics.ContainerActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	opts := &mgr.ContainerListOption{All: true}
	filter := func(c *mgr.Container) bool {
		return c.Config.Labels[containerTypeLabelKey] == containerTypeLabelContainer
	}
	opts.FilterFunc = filter

	// Filter *only* (non-sandbox) containers.
	containerList, err := c.ContainerMgr.List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to list container: %v", err)
	}

	containers := make([]*runtime.Container, 0, len(containerList))
	for _, c := range containerList {
		container, err := toCriContainer(c)
		if err != nil {
			log.With(ctx).Warnf("failed to translate container %v to cri container in ListContainers: %v", c.ID, err)
			continue
		}
		containers = append(containers, container)
	}

	result := filterCRIContainers(containers, r.GetFilter())

	metrics.ContainerSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.ListContainersResponse{Containers: result}, nil
}

// ContainerStatus inspects the container and returns the status.
func (c *CriManager) ContainerStatus(ctx context.Context, r *runtime.ContainerStatusRequest) (*runtime.ContainerStatusResponse, error) {
	label := util_metrics.ActionStatusLabel
	defer func(start time.Time) {
		metrics.ContainerActionsCounter.WithLabelValues(label).Inc()
		metrics.ContainerActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	id := r.GetContainerId()
	container, err := c.ContainerMgr.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get container status of %q: %v", id, err)
	}

	// Parse the timestamps.
	var createdAt, startedAt, finishedAt int64
	for _, item := range []struct {
		t *int64
		s string
	}{
		{t: &createdAt, s: container.Created},
		{t: &startedAt, s: container.State.StartedAt},
		{t: &finishedAt, s: container.State.FinishedAt},
	} {
		*item.t, err = toCriTimestamp(item.s)
		if err != nil {
			return nil, fmt.Errorf("failed to parse timestamp for container %q: %v", id, err)
		}
	}

	// Convert the mounts.
	mounts := make([]*runtime.Mount, 0, len(container.Mounts))
	for _, m := range container.Mounts {
		mounts = append(mounts, &runtime.Mount{
			HostPath:      m.Source,
			ContainerPath: m.Destination,
			Readonly:      !m.RW,
			Name:          m.Name,
			// Note: can't set SeLinuxRelabel.
		})
	}

	state, reason := toCriContainerState(container.State)

	metadata, err := parseContainerName(container.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to get container status of %q: %v", id, err)
	}

	labels, annotations := extractLabels(container.Config.Labels)

	// FIXME(fuwei): if user repush image with the same reference, the image
	// ID will be changed. For now, pouch daemon will remove the old image ID
	// so that CRI fails to fetch the running container. Before upgrade
	// pouch daemon image manager, we use reference to get image instead of
	// id.
	imageInfo, err := c.ImageMgr.GetImage(ctx, container.Config.Image)
	if err != nil {
		return nil, fmt.Errorf("failed to get image %s: %v", container.Config.Image, err)
	}
	imageRef := imageInfo.ID
	if len(imageInfo.RepoDigests) > 0 {
		imageRef = imageInfo.RepoDigests[0]
	}

	logPath := labels[containerLogPathLabelKey]

	resources := container.HostConfig.Resources
	diskQuota := container.Config.DiskQuota
	status := &runtime.ContainerStatus{
		Id:          container.ID,
		Metadata:    metadata,
		Image:       &runtime.ImageSpec{Image: container.Config.Image},
		ImageRef:    imageRef,
		Mounts:      mounts,
		ExitCode:    int32(container.State.ExitCode),
		State:       state,
		CreatedAt:   createdAt,
		StartedAt:   startedAt,
		FinishedAt:  finishedAt,
		Reason:      reason,
		Message:     container.State.Error,
		Labels:      labels,
		Annotations: annotations,
		LogPath:     logPath,
		Volumes:     parseVolumesFromPouch(container.Config.Volumes),
		Resources:   parseResourcesFromPouch(resources, diskQuota),
		QuotaId:     container.Config.QuotaID,
		Envs:        parseEnvsFromPouch(container.Config.Env),
	}

	metrics.ContainerSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.ContainerStatusResponse{Status: status}, nil
}

// ContainerStats returns stats of the container. If the container does not
// exist, the call returns an error.
func (c *CriManager) ContainerStats(ctx context.Context, r *runtime.ContainerStatsRequest) (*runtime.ContainerStatsResponse, error) {
	label := util_metrics.ActionStatsLabel
	defer func(start time.Time) {
		metrics.ContainerActionsCounter.WithLabelValues(label).Inc()
		metrics.ContainerActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	containerID := r.GetContainerId()

	container, err := c.ContainerMgr.Get(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("failed to get container %q with error: %v", containerID, err)
	}

	cs, err := c.getContainerMetrics(ctx, container)
	if err != nil {
		return nil, fmt.Errorf("failed to decode container metrics: %v", err)
	}

	metrics.ContainerSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.ContainerStatsResponse{Stats: cs}, nil
}

// ListContainerStats returns stats of all running containers.
func (c *CriManager) ListContainerStats(ctx context.Context, r *runtime.ListContainerStatsRequest) (*runtime.ListContainerStatsResponse, error) {
	label := util_metrics.ActionStatsListLabel
	defer func(start time.Time) {
		metrics.ContainerActionsCounter.WithLabelValues(label).Inc()
		metrics.ContainerActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	opts := &mgr.ContainerListOption{All: true}
	filter := func(c *mgr.Container) bool {
		if c.Config.Labels[containerTypeLabelKey] != containerTypeLabelContainer {
			return false
		}

		if r.GetFilter().GetId() != "" && c.ID != r.GetFilter().GetId() {
			return false
		}
		if r.GetFilter().GetPodSandboxId() != "" && c.Config.Labels[sandboxIDLabelKey] != r.GetFilter().GetPodSandboxId() {
			return false
		}
		if r.GetFilter().GetLabelSelector() != nil &&
			!utils.MatchLabelSelector(r.GetFilter().GetLabelSelector(), c.Config.Labels) {
			return false
		}
		return true
	}
	opts.FilterFunc = filter

	containers, err := c.ContainerMgr.List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %v", err)
	}

	result := &runtime.ListContainerStatsResponse{}
	for _, container := range containers {
		cs, err := c.getContainerMetrics(ctx, container)
		if err != nil {
			log.With(ctx).Warnf("failed to decode metrics of container %q: %v", container.ID, err)
			continue
		}

		result.Stats = append(result.Stats, cs)
	}

	metrics.ContainerSuccessActionsCounter.WithLabelValues(label).Inc()

	return result, nil
}

// UpdateContainerResources updates ContainerConfig of the container.
func (c *CriManager) UpdateContainerResources(ctx context.Context, r *runtime.UpdateContainerResourcesRequest) (*runtime.UpdateContainerResourcesResponse, error) {
	label := util_metrics.ActionUpdateLabel
	defer func(start time.Time) {
		metrics.ContainerActionsCounter.WithLabelValues(label).Inc()
		metrics.ContainerActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	containerID := r.GetContainerId()
	container, err := c.ContainerMgr.Get(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("failed to get container %q: %v", containerID, err)
	}

	// cannot update container resource when it is in removing state
	if container.IsRemoving() {
		return nil, fmt.Errorf("cannot to update resource for container %q when it is in removing state", containerID)
	}

	resources := r.GetLinux()
	updateConfig := &apitypes.UpdateConfig{
		Resources:      parseResourcesFromCRI(resources),
		DiskQuota:      resources.GetDiskQuota(),
		SpecAnnotation: r.GetSpecAnnotations(),
	}

	err = applyContainerConfigByAnnotation(updateConfig.SpecAnnotation, nil, nil, updateConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to apply annotation to update config: %v", err)
	}

	err = c.ContainerMgr.Update(ctx, containerID, updateConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to update resource for container %q: %v", containerID, err)
	}

	metrics.ContainerSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.UpdateContainerResourcesResponse{}, nil
}

// ReopenContainerLog asks runtime to reopen the stdout/stderr log file
// for the container. This is often called after the log file has been
// rotated. If the container is not running, container runtime can choose
// to either create a new log file and return nil, or return an error.
// Once it returns error, new container log file MUST NOT be created.
func (c *CriManager) ReopenContainerLog(ctx context.Context, r *runtime.ReopenContainerLogRequest) (*runtime.ReopenContainerLogResponse, error) {
	containerID := r.GetContainerId()

	container, err := c.ContainerMgr.Get(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("failed to get container %q with error: %v", containerID, err)
	}
	if !container.IsRunning() {
		return nil, errors.Wrap(errtypes.ErrPreCheckFailed, "container is not running")
	}

	// get logPath of container
	logPath := container.Config.Labels[containerLogPathLabelKey]
	if logPath == "" {
		log.With(ctx).Warnf("log path of container: %q is empty", containerID)
		return &runtime.ReopenContainerLogResponse{}, nil
	}

	if err := c.ContainerMgr.AttachCRILog(ctx, container.Name, logPath); err != nil {
		return nil, err
	}

	return &runtime.ReopenContainerLogResponse{}, nil
}

// ExecSync executes a command in the container, and returns the stdout output.
// If command exits with a non-zero exit code, an error is returned.
func (c *CriManager) ExecSync(ctx context.Context, r *runtime.ExecSyncRequest) (*runtime.ExecSyncResponse, error) {
	id := r.GetContainerId()

	createConfig := &apitypes.ExecCreateConfig{
		Cmd: r.GetCmd(),
	}
	execid, err := c.ContainerMgr.CreateExec(ctx, id, createConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create exec for container %q: %v", id, err)
	}

	stdoutBuf, stderrBuf := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
	attachCfg := &pkgstreams.AttachConfig{
		UseStdout: true,
		Stdout:    stdoutBuf,
		UseStderr: true,
		Stderr:    stderrBuf,
	}

	if err := c.ContainerMgr.StartExec(ctx, execid, attachCfg, int(r.GetTimeout())); err != nil {
		return nil, fmt.Errorf("failed to start exec for container %q: %v", id, err)
	}

	execConfig, err := c.ContainerMgr.GetExecConfig(ctx, execid)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect exec for container %q: %v", id, err)
	}

	return &runtime.ExecSyncResponse{
		Stdout:   stdoutBuf.Bytes(),
		Stderr:   stderrBuf.Bytes(),
		ExitCode: int32(execConfig.ExitCode),
	}, nil
}

// Exec prepares a streaming endpoint to execute a command in the container, and returns the address.
func (c *CriManager) Exec(ctx context.Context, r *runtime.ExecRequest) (*runtime.ExecResponse, error) {
	return c.StreamServer.GetExec(r)
}

// Attach prepares a streaming endpoint to attach to a running container, and returns the address.
func (c *CriManager) Attach(ctx context.Context, r *runtime.AttachRequest) (*runtime.AttachResponse, error) {
	return c.StreamServer.GetAttach(r)
}

// PortForward prepares a streaming endpoint to forward ports from a PodSandbox, and returns the address.
func (c *CriManager) PortForward(ctx context.Context, r *runtime.PortForwardRequest) (*runtime.PortForwardResponse, error) {
	return c.StreamServer.GetPortForward(r)
}

// UpdateRuntimeConfig updates the runtime config. Currently only handles podCIDR updates.
func (c *CriManager) UpdateRuntimeConfig(ctx context.Context, r *runtime.UpdateRuntimeConfigRequest) (*runtime.UpdateRuntimeConfigResponse, error) {
	podCIDR := r.GetRuntimeConfig().GetNetworkConfig().GetPodCidr()
	if podCIDR == "" {
		return &runtime.UpdateRuntimeConfigResponse{}, nil
	}

	if err := c.CniMgr.Event(cni.CNIChangeEventPodCIDR, podCIDR); err != nil {
		return nil, fmt.Errorf("failed to update podCIDR: %v", err)
	}

	log.With(ctx).Infof("CNI default PodCIDR change to \"%v\"", podCIDR)

	return &runtime.UpdateRuntimeConfigResponse{}, nil
}

// Status returns the status of the runtime.
func (c *CriManager) Status(ctx context.Context, r *runtime.StatusRequest) (*runtime.StatusResponse, error) {
	label := util_metrics.ActionStatusLabel
	// record the time spent during image pull procedure.
	defer func(start time.Time) {
		metrics.RuntimeActionsCounter.WithLabelValues(label).Inc()
		metrics.RuntimeActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	runtimeCondition := &runtime.RuntimeCondition{
		Type:   runtime.RuntimeReady,
		Status: true,
	}
	networkCondition := &runtime.RuntimeCondition{
		Type:   runtime.NetworkReady,
		Status: true,
	}

	// Check the status of the cni initialization
	if err := c.CniMgr.Status(); err != nil {
		networkCondition.Status = false
		networkCondition.Reason = networkNotReadyReason
		networkCondition.Message = fmt.Sprintf("Network plugin returns error: %v", err)
	}

	resp := &runtime.StatusResponse{
		Status: &runtime.RuntimeStatus{Conditions: []*runtime.RuntimeCondition{
			runtimeCondition,
			networkCondition,
		}},
	}

	if r.Verbose {
		resp.Info = make(map[string]string)
		versionByt, err := json.Marshal(goruntime.Version())
		if err != nil {
			return nil, err
		}
		configByt, err := json.Marshal(c.DaemonConfig)
		if err != nil {
			return nil, err
		}
		resp.Info["golang"] = string(versionByt)
		resp.Info["daemon-config"] = string(configByt)

		// TODO return more info
	}

	metrics.RuntimeSuccessActionsCounter.WithLabelValues(label).Inc()

	return resp, nil
}

// ListImages lists existing images.
func (c *CriManager) ListImages(ctx context.Context, r *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error) {
	label := util_metrics.ActionListLabel
	// record the time spent during image pull procedure.
	defer func(start time.Time) {
		metrics.ImageActionsCounter.WithLabelValues(label).Inc()
		metrics.ImageActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	// TODO: handle image list filters.
	imageList, err := c.ImageMgr.ListImages(ctx, filters.NewArgs())
	if err != nil {
		return nil, err
	}

	// We may get images with same id and different repoTag or repoDigest,
	// so we need idExist to de-dup.
	idExist := make(map[string]bool)

	images := make([]*runtime.Image, 0, len(imageList))
	for _, i := range imageList {
		if _, ok := idExist[i.ID]; ok {
			continue
		}
		// NOTE: we should query image cache to get the correct image info.
		imageInfo, err := c.ImageMgr.GetImage(ctx, i.ID)
		if err != nil {
			continue
		}
		image, err := imageToCriImage(imageInfo)
		if err != nil {
			// TODO: log an error message?
			continue
		}
		images = append(images, image)
		idExist[i.ID] = true
	}

	metrics.ImageSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.ListImagesResponse{Images: images}, nil
}

// ImageStatus returns the status of the image. If the image is not present,
// returns a response with ImageStatusResponse.Image set to nil.
func (c *CriManager) ImageStatus(ctx context.Context, r *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error) {
	label := util_metrics.ActionStatusLabel
	defer func(start time.Time) {
		metrics.ImageActionsCounter.WithLabelValues(label).Inc()
		metrics.ImageActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	imageRef := r.GetImage().GetImage()
	ref, err := reference.Parse(imageRef)
	if err != nil {
		return nil, err
	}

	imageInfo, err := c.ImageMgr.GetImage(ctx, ref.String())
	if err != nil {
		if errtypes.IsNotfound(err) {
			return &runtime.ImageStatusResponse{}, nil
		}
		return nil, err
	}

	image, err := imageToCriImage(imageInfo)
	if err != nil {
		return nil, err
	}

	metrics.ImageSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.ImageStatusResponse{Image: image}, nil
}

// PullImage pulls an image with authentication config.
func (c *CriManager) PullImage(ctx context.Context, r *runtime.PullImageRequest) (*runtime.PullImageResponse, error) {
	imageRef := r.GetImage().GetImage()

	label := util_metrics.ActionPullLabel
	// record the time spent during image pull procedure.
	defer func(start time.Time) {
		metrics.ImageActionsCounter.WithLabelValues(label).Inc()
		metrics.ImagePullSummary.WithLabelValues(imageRef).Observe(util_metrics.SinceInMicroseconds(start))
		metrics.ImageActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	authConfig := &apitypes.AuthConfig{}
	if auth := r.GetAuth(); auth != nil {
		authConfig.Auth = auth.GetAuth()
		authConfig.Username = auth.GetUsername()
		authConfig.Password = auth.GetPassword()
		authConfig.ServerAddress = auth.GetServerAddress()
		authConfig.IdentityToken = auth.GetIdentityToken()
		authConfig.RegistryToken = auth.GetRegistryToken()
	}

	if err := c.ImageMgr.PullImage(ctx, imageRef, authConfig, bytes.NewBuffer([]byte{})); err != nil {
		return nil, err
	}

	imageInfo, err := c.ImageMgr.GetImage(ctx, imageRef)
	if err != nil {
		return nil, err
	}

	metrics.ImageSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.PullImageResponse{ImageRef: imageInfo.ID}, nil
}

// RemoveImage removes the image.
func (c *CriManager) RemoveImage(ctx context.Context, r *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
	label := util_metrics.ActionRemoveLabel
	defer func(start time.Time) {
		metrics.ImageActionsCounter.WithLabelValues(label).Inc()
		metrics.ImageActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	imageRef := r.GetImage().GetImage()

	if err := c.ImageMgr.RemoveImage(ctx, imageRef, false); err != nil {
		if errtypes.IsNotfound(err) {
			// Now we just return empty if the ErrorNotFound occurred.
			return &runtime.RemoveImageResponse{}, nil
		}
		return nil, err
	}

	metrics.ImageSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.RemoveImageResponse{}, nil
}

// ImageFsInfo returns information of the filesystem that is used to store images.
func (c *CriManager) ImageFsInfo(ctx context.Context, r *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error) {
	label := util_metrics.ActionInfoLabel
	defer func(start time.Time) {
		metrics.ImageActionsCounter.WithLabelValues(label).Inc()
		metrics.ImageActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	snapshots := c.SnapshotStore.List()
	timestamp := time.Now().UnixNano()
	var usedBytes, inodesUsed uint64
	for _, sn := range snapshots {
		// Use the oldest timestamp as the timestamp of imagefs info.
		if sn.Timestamp < timestamp {
			timestamp = sn.Timestamp
		}
		usedBytes += sn.Size
		inodesUsed += sn.Inodes
	}

	metrics.ImageSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.ImageFsInfoResponse{
		ImageFilesystems: []*runtime.FilesystemUsage{
			{
				Timestamp:  timestamp,
				FsId:       &runtime.FilesystemIdentifier{Mountpoint: c.imageFSPath},
				UsedBytes:  &runtime.UInt64Value{Value: usedBytes},
				InodesUsed: &runtime.UInt64Value{Value: inodesUsed},
			},
		},
	}, nil
}

// RemoveVolume removes the volume.
func (c *CriManager) RemoveVolume(ctx context.Context, r *runtime.RemoveVolumeRequest) (*runtime.RemoveVolumeResponse, error) {
	label := util_metrics.ActionRemoveLabel
	defer func(start time.Time) {
		metrics.VolumeActionsCounter.WithLabelValues(label).Inc()
		metrics.VolumeActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	volumeName := r.GetVolumeName()
	if err := c.VolumeMgr.Remove(ctx, volumeName); err != nil {
		return nil, err
	}

	metrics.VolumeSuccessActionsCounter.WithLabelValues(label).Inc()

	return &runtime.RemoveVolumeResponse{}, nil
}
