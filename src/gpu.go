package src

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type GPU struct {
	UUID         string
	Model        string
	ResourceName string
	CDIName      string
	Health       string
}

type GPUDiscoverer interface {
	Discover(context.Context) ([]*GPU, error)
}

type HealthUpdate struct {
	UUID   string
	Health string
	Reason string
}

type HealthMonitor interface {
	Run(context.Context, chan<- HealthUpdate) error
}

type NVMLBackend struct {
	resourceDomain         string
	modelAliases           map[string]string
	advertiseUnknownModels bool
}

func NewNVMLBackend(resourceDomain string, modelAliases map[string]string, advertiseUnknownModels bool) *NVMLBackend {
	return &NVMLBackend{
		resourceDomain:         resourceDomain,
		modelAliases:           modelAliases,
		advertiseUnknownModels: advertiseUnknownModels,
	}
}

func (b *NVMLBackend) Discover(ctx context.Context) ([]*GPU, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("nvml device count: %s", nvml.ErrorString(ret))
	}

	seen := make(map[string]struct{}, count)
	gpus := make([]*GPU, 0, count)

	for i := 0; i < count; i++ {
		dev, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("nvml device[%d] handle: %s", i, nvml.ErrorString(ret))
		}

		uuid, ret := dev.GetUUID()
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("nvml device[%d] uuid: %s", i, nvml.ErrorString(ret))
		}

		if _, exists := seen[uuid]; exists {
			return nil, fmt.Errorf("duplicate GPU UUID discovered: %s", uuid)
		}
		seen[uuid] = struct{}{}

		model, ret := dev.GetName()
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("nvml device[%d] model: %s", i, nvml.ErrorString(ret))
		}

		resourceName, ok := b.resourceForModel(model)
		if !ok {
			log.Printf("WARN: unsupported GPU model=%q uuid=%s; skipping", model, uuid)
			continue
		}

		gpu := &GPU{
			UUID:         uuid,
			Model:        model,
			ResourceName: resourceName,
			CDIName:      "nvidia.com/gpu=" + uuid,
			Health:       pluginapi.Healthy,
		}

		gpus = append(gpus, gpu)

		log.Printf(
			"discovered GPU model=%q uuid=%s resource=%s cdi=%s",
			gpu.Model,
			gpu.UUID,
			gpu.ResourceName,
			gpu.CDIName,
		)
	}

	return gpus, nil
}

func (b *NVMLBackend) resourceForModel(model string) (string, bool) {
	canonical := canonicalModel(model)

	if suffix, ok := b.modelAliases[canonical]; ok {
		if !validResourceSuffix(suffix) {
			log.Printf("WARN: invalid resource suffix %q configured for model %q", suffix, model)
			return "", false
		}

		return b.resourceDomain + "/" + suffix, true
	}

	if !b.advertiseUnknownModels {
		return "", false
	}

	suffix := modelResourceSuffix(canonical)
	if !validResourceSuffix(suffix) {
		return "", false
	}

	return b.resourceDomain + "/" + suffix, true
}

func canonicalModel(model string) string {
	model = strings.TrimSpace(strings.ToLower(model))
	model = strings.TrimPrefix(model, "nvidia geforce ")
	model = strings.TrimPrefix(model, "nvidia ")
	return strings.Join(strings.Fields(model), " ")
}

func modelResourceSuffix(model string) string {
	var result strings.Builder
	lastDash := false

	for _, r := range strings.ToLower(model) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			result.WriteRune(r)
			lastDash = false
			continue
		}

		if !lastDash && result.Len() > 0 {
			result.WriteByte('-')
			lastDash = true
		}
	}

	return strings.Trim(result.String(), "-")
}

func validResourceSuffix(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}

	for i, r := range s {
		valid := (r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') ||
			r == '.' ||
			r == '-' ||
			r == '_'

		if !valid {
			return false
		}

		if (i == 0 || i == len(s)-1) &&
			!((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}

	return true
}

type NVMLHealthMonitor struct {
	mu              sync.Mutex
	known           map[string]struct{}
	unhealthySince  map[string]time.Time
	recoverySuccess map[string]int
	probeInterval   time.Duration
	recoveryDelay   time.Duration
	recoveryNeeded  int
}

func NewNVMLHealthMonitor(gpus []*GPU, probeInterval, recoveryDelay time.Duration, recoveryNeeded int) *NVMLHealthMonitor {
	known := make(map[string]struct{}, len(gpus))

	for _, gpu := range gpus {
		known[gpu.UUID] = struct{}{}
	}

	return &NVMLHealthMonitor{
		known:           known,
		unhealthySince:  make(map[string]time.Time),
		recoverySuccess: make(map[string]int),
		probeInterval:   probeInterval,
		recoveryDelay:   recoveryDelay,
		recoveryNeeded:  recoveryNeeded,
	}
}

func (m *NVMLHealthMonitor) Run(ctx context.Context, out chan<- HealthUpdate) error {
	eventSet, ret := nvml.EventSetCreate()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("nvml event set create: %s", nvml.ErrorString(ret))
	}
	defer eventSet.Free()

	for uuid := range m.known {
		dev, ret := nvml.DeviceGetHandleByUUID(uuid)

		if ret != nvml.SUCCESS {
			m.send(ctx, out, HealthUpdate{
				UUID:   uuid,
				Health: pluginapi.Unhealthy,
				Reason: "NVML handle unavailable",
			})
			continue
		}

		ret = dev.RegisterEvents(nvml.EventTypeXidCriticalError, eventSet)

		if ret != nvml.SUCCESS && ret != nvml.ERROR_NOT_SUPPORTED {
			return fmt.Errorf("register XID events for %s: %s", uuid, nvml.ErrorString(ret))
		}
	}

	ticker := time.NewTicker(m.probeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-ticker.C:
			m.probe(ctx, out)

		default:
			data, ret := eventSet.Wait(1000)

			if ret == nvml.ERROR_TIMEOUT {
				continue
			}

			if ret != nvml.SUCCESS {
				if ctx.Err() != nil {
					return nil
				}

				log.Printf("WARN: NVML event wait failed: %s", nvml.ErrorString(ret))
				continue
			}

			if data.EventType&nvml.EventTypeXidCriticalError == 0 {
				continue
			}

			uuid, ret := data.Device.GetUUID()
			if ret != nvml.SUCCESS {
				log.Printf("WARN: cannot resolve UUID for NVML health event: %s", nvml.ErrorString(ret))
				continue
			}

			m.mu.Lock()
			m.unhealthySince[uuid] = time.Now()
			m.recoverySuccess[uuid] = 0
			m.mu.Unlock()

			m.send(ctx, out, HealthUpdate{
				UUID:   uuid,
				Health: pluginapi.Unhealthy,
				Reason: fmt.Sprintf("NVML XID critical error: %d", data.EventData),
			})
		}
	}
}

func (m *NVMLHealthMonitor) probe(ctx context.Context, out chan<- HealthUpdate) {
	for uuid := range m.known {
		dev, ret := nvml.DeviceGetHandleByUUID(uuid)
		healthy := ret == nvml.SUCCESS

		if healthy {
			_, ret = dev.GetUUID()
			healthy = ret == nvml.SUCCESS
		}

		m.mu.Lock()

		since, wasUnhealthy := m.unhealthySince[uuid]

		if !healthy {
			if !wasUnhealthy {
				m.unhealthySince[uuid] = time.Now()
			}

			m.recoverySuccess[uuid] = 0
			m.mu.Unlock()

			m.send(ctx, out, HealthUpdate{
				UUID:   uuid,
				Health: pluginapi.Unhealthy,
				Reason: "NVML probe failed",
			})
			continue
		}

		if !wasUnhealthy {
			m.mu.Unlock()
			continue
		}

		if time.Since(since) < m.recoveryDelay {
			m.mu.Unlock()
			continue
		}

		m.recoverySuccess[uuid]++

		if m.recoverySuccess[uuid] < m.recoveryNeeded {
			m.mu.Unlock()
			continue
		}

		delete(m.unhealthySince, uuid)
		delete(m.recoverySuccess, uuid)
		m.mu.Unlock()

		m.send(ctx, out, HealthUpdate{
			UUID:   uuid,
			Health: pluginapi.Healthy,
			Reason: "NVML probes recovered",
		})
	}
}

func (m *NVMLHealthMonitor) send(ctx context.Context, out chan<- HealthUpdate, update HealthUpdate) {
	select {
	case out <- update:
	case <-ctx.Done():
	}
}
