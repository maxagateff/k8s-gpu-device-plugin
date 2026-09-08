package src

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type Registrar interface {
	Register(context.Context, string, string, *pluginapi.DevicePluginOptions) error
}

type KubeletRegistrar struct{ pluginPath string }

func (r *KubeletRegistrar) Register(ctx context.Context, endpoint, resourceName string, options *pluginapi.DevicePluginOptions) error {
	kubeletSocket := filepath.Join(r.pluginPath, filepath.Base(pluginapi.KubeletSocket))

	if err := checkSocket(kubeletSocket); err != nil {
		return fmt.Errorf("kubelet socket: %w", err)
	}

	conn, err := grpc.DialContext(
		ctx,
		"unix://"+kubeletSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("kubelet unavailable at %s: %w", kubeletSocket, err)
	}
	defer conn.Close()

	client := pluginapi.NewRegistrationClient(conn)

	_, err = client.Register(ctx, &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     endpoint,
		ResourceName: resourceName,
		Options:      options,
	})
	if err != nil {
		return fmt.Errorf("registration failed for %s: %w", resourceName, err)
	}

	return nil
}

type devicePlugin struct {
	pluginapi.UnimplementedDevicePluginServer

	resourceName string
	socketName   string
	pluginPath   string
	registrar    Registrar
	cdi          CDIResolver

	mu      sync.RWMutex
	devices map[string]*GPU
	updates chan struct{}

	lifecycleMu sync.Mutex
	server      *grpc.Server
	listener    net.Listener
	started     bool
}

func newPlugin(resourceName, socketName, pluginPath string, gpus []*GPU, registrar Registrar, cdi CDIResolver) (*devicePlugin, error) {
	if resourceName == "" {
		return nil, errors.New("empty resource name")
	}

	if len(gpus) == 0 {
		return nil, fmt.Errorf("resource %s has no GPUs", resourceName)
	}

	devices := make(map[string]*GPU, len(gpus))

	for _, gpu := range gpus {
		if gpu.ResourceName != resourceName {
			return nil, fmt.Errorf(
				"GPU %s belongs to %s, not %s",
				gpu.UUID,
				gpu.ResourceName,
				resourceName,
			)
		}

		if _, exists := devices[gpu.UUID]; exists {
			return nil, fmt.Errorf("duplicate UUID %s in resource %s", gpu.UUID, resourceName)
		}

		devices[gpu.UUID] = gpu
	}

	return &devicePlugin{
		resourceName: resourceName,
		socketName:   socketName,
		pluginPath:   pluginPath,
		registrar:    registrar,
		cdi:          cdi,
		devices:      devices,
		updates:      make(chan struct{}, 1),
	}, nil
}

func pluginOptions() *pluginapi.DevicePluginOptions {
	return &pluginapi.DevicePluginOptions{
		PreStartRequired:                false,
		GetPreferredAllocationAvailable: false,
	}
}

func (p *devicePlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return pluginOptions(), nil
}

func (p *devicePlugin) ListAndWatch(_ *pluginapi.Empty, stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	if err := p.sendDevices(stream); err != nil {
		return err
	}

	for {
		select {
		case <-stream.Context().Done():
			return nil

		case <-p.updates:
			if err := p.sendDevices(stream); err != nil {
				return err
			}
		}
	}
}

func (p *devicePlugin) sendDevices(stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	p.mu.RLock()

	devices := make([]*pluginapi.Device, 0, len(p.devices))

	for _, gpu := range p.devices {
		devices = append(devices, &pluginapi.Device{
			ID:     gpu.UUID,
			Health: gpu.Health,
		})
	}

	p.mu.RUnlock()

	sort.Slice(devices, func(i, j int) bool {
		return devices[i].ID < devices[j].ID
	})

	return stream.Send(&pluginapi.ListAndWatchResponse{Devices: devices})
}

func (p *devicePlugin) Allocate(_ context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	response := &pluginapi.AllocateResponse{}

	for _, containerReq := range req.ContainerRequests {
		if len(containerReq.DevicesIds) == 0 {
			return nil, fmt.Errorf("%s: empty device request", p.resourceName)
		}

		containerResp := &pluginapi.ContainerAllocateResponse{}
		seen := make(map[string]struct{}, len(containerReq.DevicesIds))

		for _, id := range containerReq.DevicesIds {
			if _, duplicate := seen[id]; duplicate {
				return nil, fmt.Errorf("%s: duplicate device requested: %s", p.resourceName, id)
			}

			seen[id] = struct{}{}

			p.mu.RLock()

			gpu, ok := p.devices[id]

			if ok {
				copyGPU := *gpu
				gpu = &copyGPU
			}

			p.mu.RUnlock()

			if !ok {
				return nil, fmt.Errorf("%s: unknown device %s", p.resourceName, id)
			}

			if gpu.ResourceName != p.resourceName {
				return nil, fmt.Errorf(
					"%s: device %s belongs to %s",
					p.resourceName,
					id,
					gpu.ResourceName,
				)
			}

			if gpu.Health != pluginapi.Healthy {
				return nil, fmt.Errorf(
					"%s: device %s is %s",
					p.resourceName,
					id,
					gpu.Health,
				)
			}

			if err := p.cdi.Exists(gpu.CDIName); err != nil {
				return nil, fmt.Errorf(
					"%s: CDI unavailable for %s: %w",
					p.resourceName,
					id,
					err,
				)
			}

			containerResp.CdiDevices = append(
				containerResp.CdiDevices,
				&pluginapi.CDIDevice{Name: gpu.CDIName},
			)

			log.Printf(
				"%s: allocating uuid=%s cdi=%s",
				p.resourceName,
				id,
				gpu.CDIName,
			)
		}

		response.ContainerResponses = append(response.ContainerResponses, containerResp)
	}

	return response, nil
}

func (p *devicePlugin) GetPreferredAllocation(context.Context, *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return nil, fmt.Errorf("preferred allocation is not supported")
}

func (p *devicePlugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return nil, fmt.Errorf("pre-start container is not required")
}

func (p *devicePlugin) start(ctx context.Context) (err error) {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()

	if p.started {
		return fmt.Errorf("%s: plugin already started", p.resourceName)
	}

	if err := ensurePluginDir(p.pluginPath); err != nil {
		return err
	}

	socketPath := filepath.Join(p.pluginPath, p.socketName)

	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale socket %s: %w", socketPath, err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", socketPath, err)
	}

	p.listener = listener
	p.server = grpc.NewServer()

	pluginapi.RegisterDevicePluginServer(p.server, p)

	serveErr := make(chan error, 1)
	go func() { serveErr <- p.server.Serve(listener) }()

	cleanupOnError := true

	defer func() {
		if cleanupOnError {
			p.stopLocked()
		}
	}()

	if err := waitForSocket(ctx, socketPath); err != nil {
		return fmt.Errorf("%s: gRPC endpoint unavailable: %w", p.resourceName, err)
	}

	regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := p.registrar.Register(regCtx, p.socketName, p.resourceName, pluginOptions()); err != nil {
		return err
	}

	select {
	case err := <-serveErr:
		return fmt.Errorf("%s: gRPC server stopped during start: %w", p.resourceName, err)
	default:
	}

	p.started = true
	cleanupOnError = false

	log.Printf(
		"registered resource=%s socket=%s devices=%d",
		p.resourceName,
		socketPath,
		len(p.devices),
	)

	return nil
}

func (p *devicePlugin) reregister(ctx context.Context) error {
	p.lifecycleMu.Lock()
	started := p.started
	p.lifecycleMu.Unlock()

	if !started {
		return fmt.Errorf("%s: cannot re-register stopped plugin", p.resourceName)
	}

	regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := p.registrar.Register(regCtx, p.socketName, p.resourceName, pluginOptions()); err != nil {
		return err
	}

	log.Printf("re-registered resource=%s", p.resourceName)

	return nil
}

func (p *devicePlugin) setHealth(uuid, health, reason string) {
	p.mu.Lock()

	gpu, ok := p.devices[uuid]

	if !ok || gpu.Health == health {
		p.mu.Unlock()
		return
	}

	old := gpu.Health
	gpu.Health = health

	p.mu.Unlock()

	log.Printf(
		"GPU health changed uuid=%s resource=%s %s->%s reason=%s",
		uuid,
		p.resourceName,
		old,
		health,
		reason,
	)

	p.notify()
}

func (p *devicePlugin) notify() {
	select {
	case p.updates <- struct{}{}:
	default:
	}
}

func (p *devicePlugin) socketExists() bool {
	_, err := os.Stat(filepath.Join(p.pluginPath, p.socketName))
	return err == nil
}

func (p *devicePlugin) stop() {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()

	p.stopLocked()
}

func (p *devicePlugin) stopLocked() {
	if p.server != nil {
		p.server.Stop()
		p.server = nil
	}

	if p.listener != nil {
		_ = p.listener.Close()
		p.listener = nil
	}

	_ = os.Remove(filepath.Join(p.pluginPath, p.socketName))

	p.started = false
}

func waitForSocket(parent context.Context, path string) error {
	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(parent, 500*time.Millisecond)

		conn, err := grpc.DialContext(
			ctx,
			"unix://"+path,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithBlock(),
		)

		cancel()

		if err == nil {
			_ = conn.Close()
			return nil
		}

		select {
		case <-parent.Done():
			return parent.Err()

		case <-time.After(100 * time.Millisecond):
		}
	}

	return fmt.Errorf("timeout waiting for socket %s", path)
}

func ensurePluginDir(path string) error {
	info, err := os.Stat(path)

	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("device-plugin directory %s does not exist", path)
		}

		if os.IsPermission(err) {
			return fmt.Errorf("permission denied accessing device-plugin directory %s: %w", path, err)
		}

		return fmt.Errorf("stat device-plugin directory %s: %w", path, err)
	}

	if !info.IsDir() {
		return fmt.Errorf("device-plugin path %s is not a directory", path)
	}

	return nil
}

func checkSocket(path string) error {
	info, err := os.Stat(path)

	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s does not exist", path)
		}

		if os.IsPermission(err) {
			return fmt.Errorf("permission denied: %w", err)
		}

		return err
	}

	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists but is not a Unix socket", path)
	}

	return nil
}
