package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

type Config struct {
	ResourceDomain          string
	PluginPath              string
	HealthProbeInterval     time.Duration
	HealthRecoveryDelay     time.Duration
	HealthRecoverySuccesses int
	AdvertiseUnknownModels  bool
	ModelAliases            map[string]string
}

func defaultConfig() Config {
	return Config{
		ResourceDomain:          envString("GPU_RESOURCE_DOMAIN", "gpu.local"),
		PluginPath:              envString("GPU_PLUGIN_PATH", "/var/lib/kubelet/device-plugins"),
		HealthProbeInterval:     envDuration("GPU_HEALTH_PROBE_INTERVAL", 5*time.Second),
		HealthRecoveryDelay:     envDuration("GPU_HEALTH_RECOVERY_DELAY", 30*time.Second),
		HealthRecoverySuccesses: envInt("GPU_HEALTH_RECOVERY_SUCCESSES", 3),
		AdvertiseUnknownModels:  true,
		ModelAliases:            map[string]string{},
	}
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}

	duration, err := time.ParseDuration(value)
	if err != nil {
		log.Printf("WARN: invalid %s=%q, using %s", name, value, fallback)
		return fallback
	}

	return duration
}

func envInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}

	number, err := strconv.Atoi(value)
	if err != nil {
		log.Printf("WARN: invalid %s=%q, using %d", name, value, fallback)
		return fallback
	}

	return number
}

func run() error {
	cfg := defaultConfig()

	if err := ensurePluginDir(cfg.PluginPath); err != nil {
		return err
	}

	if ret := nvml.Init(); ret != nvml.SUCCESS {
		return fmt.Errorf("initialize NVML: %s", nvml.ErrorString(ret))
	}

	defer func() {
		if ret := nvml.Shutdown(); ret != nvml.SUCCESS {
			log.Printf("WARN: NVML shutdown failed: %s", nvml.ErrorString(ret))
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	nvmlBackend := NewNVMLBackend(cfg.ResourceDomain, cfg.ModelAliases, cfg.AdvertiseUnknownModels)
	registrar := &KubeletRegistrar{pluginPath: cfg.PluginPath}
	cdiResolver := NewCDICacheResolver()
	manager := NewManager(cfg, nvmlBackend, registrar, cdiResolver)

	log.Printf("k8s-gpu-device-plugin starting domain=%s pluginPath=%s", cfg.ResourceDomain, cfg.PluginPath)

	return manager.Run(ctx)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if err := run(); err != nil {
		log.Printf("FATAL: %v", err)
		os.Exit(1)
	}

	log.Printf("k8s-gpu-device-plugin stopped")
}
