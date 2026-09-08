package main

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type Manager struct {
	cfg        Config
	discoverer GPUDiscoverer
	registrar  Registrar
	cdi        CDIResolver

	mu      sync.RWMutex
	plugins map[string]*devicePlugin
	byUUID  map[string]*devicePlugin
}

func NewManager(cfg Config, discoverer GPUDiscoverer, registrar Registrar, cdi CDIResolver) *Manager {
	return &Manager{
		cfg:        cfg,
		discoverer: discoverer,
		registrar:  registrar,
		cdi:        cdi,
		plugins:    make(map[string]*devicePlugin),
		byUUID:     make(map[string]*devicePlugin),
	}
}

func (m *Manager) Run(ctx context.Context) error {
	if err := ensurePluginDir(m.cfg.PluginPath); err != nil {
		return err
	}

	gpus, err := m.discoverer.Discover(ctx)
	if err != nil {
		return fmt.Errorf("GPU discovery failed: %w", err)
	}

	if len(gpus) == 0 {
		return fmt.Errorf("no supported NVIDIA GPUs discovered")
	}

	if err := m.buildAndStart(ctx, gpus); err != nil {
		return err
	}
	defer m.stopAll()

	healthUpdates := make(chan HealthUpdate, 32)

	healthCtx, cancelHealth := context.WithCancel(ctx)
	defer cancelHealth()

	health := NewNVMLHealthMonitor(
		gpus,
		m.cfg.HealthProbeInterval,
		m.cfg.HealthRecoveryDelay,
		m.cfg.HealthRecoverySuccesses,
	)

	healthErr := make(chan error, 1)
	go func() { healthErr <- health.Run(healthCtx, healthUpdates) }()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create filesystem watcher: %w", err)
	}
	defer watcher.Close()

	if err := watcher.Add(m.cfg.PluginPath); err != nil {
		return fmt.Errorf("watch %s: %w", m.cfg.PluginPath, err)
	}

	kubeletSocket := filepath.Join(m.cfg.PluginPath, pluginapi.KubeletSocket)

	for {
		select {
		case <-ctx.Done():
			log.Printf("shutdown requested")
			return nil

		case err := <-healthErr:
			if err != nil && ctx.Err() == nil {
				return fmt.Errorf("health monitor failed: %w", err)
			}
			return nil

		case update := <-healthUpdates:
			m.handleHealthUpdate(update)

		case event := <-watcher.Events:
			if event.Name == kubeletSocket && event.Op&fsnotify.Create != 0 {
				log.Printf("kubelet socket recreated; re-registering resources")

				if err := m.reregisterAll(ctx); err != nil {
					log.Printf("ERROR: kubelet re-registration failed: %v", err)
				}

				continue
			}

			m.handlePluginSocketEvent(ctx, event)

		case err := <-watcher.Errors:
			if err != nil {
				log.Printf("WARN: filesystem watcher: %v", err)
			}
		}
	}
}

func (m *Manager) buildAndStart(ctx context.Context, gpus []*GPU) error {
	groups := make(map[string][]*GPU)
	seen := make(map[string]string)

	for _, gpu := range gpus {
		if gpu == nil || gpu.UUID == "" || gpu.ResourceName == "" {
			continue
		}

		if previous, exists := seen[gpu.UUID]; exists {
			return fmt.Errorf(
				"GPU %s would be advertised by both %s and %s",
				gpu.UUID,
				previous,
				gpu.ResourceName,
			)
		}

		seen[gpu.UUID] = gpu.ResourceName

		if err := m.cdi.Exists(gpu.CDIName); err != nil {
			log.Printf(
				"WARN: skipping GPU uuid=%s resource=%s: %v",
				gpu.UUID,
				gpu.ResourceName,
				err,
			)

			continue
		}

		groups[gpu.ResourceName] = append(groups[gpu.ResourceName], gpu)
	}

	if len(groups) == 0 {
		return fmt.Errorf("no GPUs have usable CDI devices")
	}

	resources := make([]string, 0, len(groups))

	for resource := range groups {
		resources = append(resources, resource)
	}

	sort.Strings(resources)

	started := make([]*devicePlugin, 0, len(resources))

	for _, resource := range resources {
		socketName := socketNameForResource(resource)

		p, err := newPlugin(
			resource,
			socketName,
			m.cfg.PluginPath,
			groups[resource],
			m.registrar,
			m.cdi,
		)
		if err != nil {
			m.stopPlugins(started)
			return err
		}

		if err := p.start(ctx); err != nil {
			m.stopPlugins(started)
			return fmt.Errorf("start plugin %s: %w", resource, err)
		}

		started = append(started, p)

		m.mu.Lock()

		m.plugins[resource] = p

		for _, gpu := range groups[resource] {
			if _, exists := m.byUUID[gpu.UUID]; exists {
				m.mu.Unlock()
				m.stopPlugins(started)
				return fmt.Errorf("GPU %s mapped to multiple plugins", gpu.UUID)
			}

			m.byUUID[gpu.UUID] = p
		}

		m.mu.Unlock()
	}

	return nil
}

func (m *Manager) handleHealthUpdate(update HealthUpdate) {
	m.mu.RLock()
	p := m.byUUID[update.UUID]
	m.mu.RUnlock()

	if p == nil {
		log.Printf("WARN: health update for unknown GPU uuid=%s", update.UUID)
		return
	}

	p.setHealth(update.UUID, update.Health, update.Reason)
}

func (m *Manager) reregisterAll(ctx context.Context) error {
	m.mu.RLock()

	plugins := make([]*devicePlugin, 0, len(m.plugins))

	for _, p := range m.plugins {
		plugins = append(plugins, p)
	}

	m.mu.RUnlock()

	sort.Slice(plugins, func(i, j int) bool {
		return plugins[i].resourceName < plugins[j].resourceName
	})

	for _, p := range plugins {
		if !p.socketExists() {
			return fmt.Errorf("%s: plugin socket does not exist", p.resourceName)
		}

		if err := p.reregister(ctx); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) handlePluginSocketEvent(ctx context.Context, event fsnotify.Event) {
	if event.Op&(fsnotify.Remove|fsnotify.Rename) == 0 {
		return
	}

	m.mu.RLock()

	plugins := make([]*devicePlugin, 0, len(m.plugins))

	for _, p := range m.plugins {
		plugins = append(plugins, p)
	}

	m.mu.RUnlock()

	for _, p := range plugins {
		path := filepath.Join(m.cfg.PluginPath, p.socketName)

		if path != event.Name {
			continue
		}

		log.Printf(
			"WARN: plugin socket removed resource=%s path=%s; restoring",
			p.resourceName,
			event.Name,
		)

		p.stop()

		if err := p.start(ctx); err != nil {
			log.Printf(
				"ERROR: failed to restore plugin resource=%s: %v",
				p.resourceName,
				err,
			)
		}
	}
}

func (m *Manager) stopPlugins(plugins []*devicePlugin) {
	for i := len(plugins) - 1; i >= 0; i-- {
		plugins[i].stop()
	}
}

func (m *Manager) stopAll() {
	m.mu.RLock()

	plugins := make([]*devicePlugin, 0, len(m.plugins))

	for _, p := range m.plugins {
		plugins = append(plugins, p)
	}

	m.mu.RUnlock()

	m.stopPlugins(plugins)
}

func socketNameForResource(resourceName string) string {
	parts := strings.SplitN(resourceName, "/", 2)
	name := resourceName

	if len(parts) == 2 {
		name = parts[1]
	}

	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "_", "-")
	name = strings.ReplaceAll(name, ".", "-")
	name = strings.ReplaceAll(name, "/", "-")

	return "k8s-gpu-" + name + ".sock"
}
