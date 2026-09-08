# k8s-gpu-device-plugin

`k8s-gpu-device-plugin` is a Kubernetes Device Plugin for NVIDIA GPUs that exposes individual GPU models as separate Kubernetes extended resources.

Instead of advertising every GPU under a single resource such as:

```text
nvidia.com/gpu
```

the plugin discovers the actual GPU model installed on the node and registers a resource for each model:

```text
gpu.local/rtx-3060-ti
gpu.local/gtx-1660-super
gpu.local/rtx-4090
gpu.local/a100-sxm4-40gb
```

This allows workloads to request a specific GPU model using standard K8s resource limits.

For example:

```yaml
resources:
  limits:
    gpu.local/rtx-4090: 1
```

The plugin handles GPU discovery, UUID tracking, model grouping, kubelet registration, allocation validation, CDI device injection, health monitoring, and socket recovery.

---

# Table of Contents

- [Overview](#overview)
- [Why This Exists](#why-this-exists)
- [Features](#features)
- [Architecture](#architecture)
- [Requirements](#requirements)
- [Build](#build)
- [Configuration](#configuration)
- [Running the Plugin](#running-the-plugin)
- [GPU Discovery](#gpu-discovery)
- [Resource Naming](#resource-naming)
- [Kubernetes Registration](#kubernetes-registration)
- [Using GPUs in Workloads](#using-gpus-in-workloads)
- [Multi-GPU Allocation](#multi-gpu-allocation)
- [Allocation Flow](#allocation-flow)
- [CDI Integration](#cdi-integration)
- [Health Monitoring](#health-monitoring)
- [kubelet Restart Handling](#kubelet-restart-handling)
- [Plugin Socket Recovery](#plugin-socket-recovery)
- [Duplicate GPU Protection](#duplicate-gpu-protection)
- [Graceful Shutdown](#graceful-shutdown)
- [Project Structure](#project-structure)
- [Operational Checks](#operational-checks)
- [Troubleshooting](#troubleshooting)
- [Known Limitations](#known-limitations)
- [Security Notes](#security-notes)
- [Production Deployment](#production-deployment)
- [Quick Start](#quick-start)

---

# Overview

The plugin uses NVIDIA Management Library (NVML) to discover physical NVIDIA GPUs installed on a K8s node.

For each GPU, it determines:

- GPU UUID
- GPU model
- Kubernetes resource name
- NVIDIA CDI device name
- current health state

Example physical node:

```text
GPU 0: NVIDIA GeForce RTX 3060 Ti
GPU 1: NVIDIA GeForce RTX 3060 Ti
GPU 2: NVIDIA GeForce GTX 1660 SUPER
```

The plugin groups those devices by model:

```text
gpu.local/rtx-3060-ti
├── GPU-UUID-A
└── GPU-UUID-B

gpu.local/gtx-1660-super
└── GPU-UUID-C
```

Kubernetes then sees:

```text
gpu.local/rtx-3060-ti:    2
gpu.local/gtx-1660-super: 1
```

The core invariant is:

```text
1 Device Plugin instance
=
1 Kubernetes resourceName
=
1..N physical GPUs of that model
```

A single physical GPU UUID must never be advertised by more than one resource.

---

# Why This Exists

The standard GPU resource model usually exposes NVIDIA GPUs through a generic resource:

```text
nvidia.com/gpu
```

That works well when the exact GPU model doesn't matter.

It becomes less useful on heterogeneous nodes or clusters.

For example, consider a node with:

```text
1x RTX 4090
2x RTX 3060 Ti
1x GTX 1660 SUPER
```

A workload requesting:

```yaml
resources:
  limits:
    nvidia.com/gpu: 1
```

doesn't express which GPU model it actually needs.

This plugin exposes the models separately:

```text
gpu.local/rtx-4090:       1
gpu.local/rtx-3060-ti:    2
gpu.local/gtx-1660-super: 1
```

A workload can then explicitly request:

```yaml
resources:
  limits:
    gpu.local/rtx-4090: 1
```

This is useful for mixed GPU environments where model, VRAM size, compute capability, or workload placement matters.

---

# Features

Current functionality includes:

- automatic NVIDIA GPU discovery via NVML
- no hardcoded GPU UUIDs
- no hardcoded GPU model list required
- automatic model-to-resource conversion
- configurable resource domain
- one Device Plugin endpoint per GPU model
- multiple physical GPUs per resource
- multi-GPU allocation
- UUID validation
- resource ownership validation
- GPU health validation
- CDI device validation
- NVIDIA CDI allocation
- NVML XID monitoring
- periodic health probes
- `ListAndWatch` health updates
- kubelet socket monitoring
- automatic kubelet re-registration
- plugin socket recovery
- duplicate UUID protection
- startup rollback
- SIGINT/SIGTERM handling
- graceful gRPC shutdown
- testable interfaces for discovery, registration, and CDI resolution

---

# Architecture

At a high level:

```text
+-----------------------+
|    NVIDIA Driver      |
+-----------+-----------+
            |
            v
+-----------------------+
|         NVML          |
+-----------+-----------+
            |
            v
+-----------------------+
|     GPU Discovery     |
|                       |
| UUID                   |
| Model                  |
| Health                 |
+-----------+-----------+
            |
            v
+-----------------------+
|   Model Normalizer    |
+-----------+-----------+
            |
            v
+-----------------------+
|   Resource Grouper    |
+-----------+-----------+
            |
       +----+----+
       |         |
       v         v

gpu.local/       gpu.local/
rtx-3060-ti      rtx-4090

GPU-A            GPU-C
GPU-B

       |         |
       v         v

Device Plugin   Device Plugin
gRPC endpoint   gRPC endpoint

       \         /
        \       /
         v     v

+-----------------------+
|        kubelet        |
+-----------+-----------+
            |
            v
+-----------------------+
|   Container Runtime   |
+-----------+-----------+
            |
            v
+-----------------------+
|     NVIDIA CDI        |
+-----------+-----------+
            |
            v
+-----------------------+
|       Container       |
+-----------------------+
```

The `Manager` owns the plugin lifecycle.

Its main responsibilities are:

```text
Discover GPUs
    |
    v
Validate CDI devices
    |
    v
Group GPUs by resource
    |
    v
Create Device Plugins
    |
    v
Start gRPC endpoints
    |
    v
Register with kubelet
    |
    +------> Monitor GPU health
    |
    +------> Monitor kubelet.sock
    |
    +------> Monitor plugin sockets
    |
    v
Graceful shutdown
```

---

# Requirements

The target K8s GPU node needs:

- Linux
- Kubernetes / kubelet
- NVIDIA GPU
- NVIDIA driver
- NVML
- NVIDIA Container Toolkit
- NVIDIA CDI configuration
- a CDI-capable container runtime
- access to the kubelet Device Plugin directory

The default Device Plugin directory is:

```text
/var/lib/kubelet/device-plugins
```

## Check the NVIDIA driver

```bash
nvidia-smi
```

## List physical GPUs

```bash
nvidia-smi -L
```

Example:

```text
GPU 0: NVIDIA GeForce RTX 3060 Ti (UUID: GPU-...)
GPU 1: NVIDIA GeForce GTX 1660 SUPER (UUID: GPU-...)
```

## Check CDI

```bash
nvidia-ctk cdi list
```

The output should contain GPU-specific devices similar to:

```text
nvidia.com/gpu=GPU-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
```

The plugin expects CDI device names in this format:

```text
nvidia.com/gpu=<GPU-UUID>
```

---

# Build

Clone the repo:

```bash
git clone <repository-url>
cd k8s-gpu-device-plugin
```

Resolve dependencies:

```bash
go mod tidy
```

Run static checks:

```bash
go vet ./...
```

Run tests:

```bash
go test ./...
```

Build:

```bash
go build -o k8s-gpu-device-plugin .
```

Verify the binary:

```bash
ls -lh k8s-gpu-device-plugin
file k8s-gpu-device-plugin
```

A normal local validation cycle is:

```bash
go fmt ./...
go mod tidy
go vet ./...
go test ./...
go build -o k8s-gpu-device-plugin .
```

---

# Configuration

The plugin is configured through env vars.

| Env Var | Default | Description |
|---|---|---|
| `GPU_RESOURCE_DOMAIN` | `gpu.local` | Domain used for K8s extended resources |
| `GPU_PLUGIN_PATH` | `/var/lib/kubelet/device-plugins` | kubelet Device Plugin directory |
| `GPU_HEALTH_PROBE_INTERVAL` | `5s` | Interval between NVML health probes |
| `GPU_HEALTH_RECOVERY_DELAY` | `30s` | Minimum delay before a GPU can recover |
| `GPU_HEALTH_RECOVERY_SUCCESSES` | `3` | Successful probes required before returning to `Healthy` |

Example:

```bash
GPU_RESOURCE_DOMAIN=example.com \
GPU_HEALTH_PROBE_INTERVAL=5s \
GPU_HEALTH_RECOVERY_DELAY=30s \
GPU_HEALTH_RECOVERY_SUCCESSES=3 \
./k8s-gpu-device-plugin
```

## Resource domain

The resource domain should be changed for real deployments.

For example:

```bash
GPU_RESOURCE_DOMAIN=gpu.example.com
```

Resources will then look like:

```text
gpu.example.com/rtx-4090
gpu.example.com/rtx-3060-ti
gpu.example.com/a100-sxm4-40gb
```

Use a domain controlled by your org when deploying this in a shared or production cluster.

---

# Running the Plugin

For a direct node-level test:

```bash
sudo GPU_RESOURCE_DOMAIN=gpu.local ./k8s-gpu-device-plugin
```

The process needs access to:

```text
/var/lib/kubelet/device-plugins
```

Typical startup logs should look similar to:

```text
k8s-gpu-device-plugin starting domain=gpu.local pluginPath=/var/lib/kubelet/device-plugins

discovered GPU model="NVIDIA GeForce RTX 3060 Ti" uuid=GPU-... resource=gpu.local/rtx-3060-ti cdi=nvidia.com/gpu=GPU-...

registered resource=gpu.local/rtx-3060-ti socket=/var/lib/kubelet/device-plugins/k8s-gpu-rtx-3060-ti.sock devices=1
```

The exact UUIDs and resource names depend on the GPUs installed on the node.

---

# GPU Discovery

GPU discovery happens through NVML.

The plugin calls NVML to enumerate physical GPU devices and obtains:

```text
index -> NVML device -> UUID -> model
```

Conceptually:

```text
DeviceGetCount()
      |
      v
DeviceGetHandleByIndex(i)
      |
      +----> GetUUID()
      |
      +----> GetName()
```

A discovered GPU is represented internally as:

```text
GPU
├── UUID
├── Model
├── ResourceName
├── CDIName
└── Health
```

Example:

```text
UUID:
GPU-c0a1bd7d-....

Model:
NVIDIA GeForce RTX 3060 Ti

ResourceName:
gpu.local/rtx-3060-ti

CDIName:
nvidia.com/gpu=GPU-c0a1bd7d-....

Health:
Healthy
```

Duplicate UUIDs are rejected during discovery.

---

# Resource Naming

GPU model names are normalized into valid resource suffixes.

For example:

```text
NVIDIA GeForce RTX 4090
```

becomes:

```text
rtx-4090
```

With:

```bash
GPU_RESOURCE_DOMAIN=example.com
```

the final K8s resource is:

```text
example.com/rtx-4090
```

Additional examples:

```text
NVIDIA GeForce RTX 3060 Ti
-> example.com/rtx-3060-ti

NVIDIA GeForce GTX 1660 SUPER
-> example.com/gtx-1660-super

NVIDIA A100-SXM4-40GB
-> example.com/a100-sxm4-40gb
```

The NVIDIA/GeForce prefixes aren't required in the final resource name because the resource domain already identifies the resource namespace.

---

# Kubernetes Registration

Each GPU model gets its own Device Plugin gRPC endpoint.

Example:

```text
gpu.local/rtx-3060-ti
```

uses a socket similar to:

```text
/var/lib/kubelet/device-plugins/k8s-gpu-rtx-3060-ti.sock
```

Another resource:

```text
gpu.local/gtx-1660-super
```

uses:

```text
/var/lib/kubelet/device-plugins/k8s-gpu-gtx-1660-super.sock
```

The plugin registers each endpoint with kubelet using the Kubernetes Device Plugin API.

After registration, check the node:

```bash
kubectl describe node <node-name>
```

You should see resources under `Capacity` and `Allocatable`.

Example:

```text
Capacity:
  gpu.local/gtx-1660-super: 1
  gpu.local/rtx-3060-ti:    2

Allocatable:
  gpu.local/gtx-1660-super: 1
  gpu.local/rtx-3060-ti:    2
```

You can also inspect the node directly:

```bash
kubectl get node <node-name> -o json | jq '.status.capacity, .status.allocatable'
```

---

# Using GPUs in Workloads

A workload requests a specific GPU model using standard K8s resource limits.

Example:

```yaml
apiVersion: v1
kind: Pod

metadata:
  name: gpu-test

spec:
  restartPolicy: Never

  containers:
    - name: cuda
      image: nvidia/cuda:12.1.0-base-ubuntu22.04

      command:
        - bash
        - -c
        - |
          nvidia-smi -L
          sleep 10

      resources:
        limits:
          gpu.local/rtx-3060-ti: 1
```

Apply it:

```bash
kubectl apply -f pod.yaml
```

Check the Pod:

```bash
kubectl get pod gpu-test -o wide
```

Read the logs:

```bash
kubectl logs gpu-test
```

Expected output:

```text
GPU 0: NVIDIA GeForce RTX 3060 Ti (...)
```

The container should only receive the GPU selected by kubelet/CDI for that allocation.

---

# Multi-GPU Allocation

Multiple GPUs of the same model are exposed under one resource.

Example node:

```text
GPU-A -> RTX 4090
GPU-B -> RTX 4090
GPU-C -> RTX 4090
GPU-D -> RTX 4090
```

Kubernetes sees:

```text
gpu.local/rtx-4090: 4
```

A workload can request two:

```yaml
resources:
  limits:
    gpu.local/rtx-4090: 2
```

kubelet selects two device IDs and sends them to `Allocate()`.

Conceptually:

```text
AllocateRequest
    |
    +---- GPU-UUID-A
    |
    +---- GPU-UUID-C
```

The plugin validates both and returns:

```text
nvidia.com/gpu=GPU-UUID-A
nvidia.com/gpu=GPU-UUID-C
```

through CDI.

---

# Allocation Flow

The full allocation path is:

```text
Pod
 |
 | requests:
 | gpu.local/rtx-4090: 1
 |
 v
K8s Scheduler
 |
 v
kubelet
 |
 | selects physical device UUID
 |
 v
Device Plugin
 |
 | Allocate()
 |
 +----> UUID exists?
 |
 +----> UUID belongs to this resource?
 |
 +----> GPU Healthy?
 |
 +----> duplicate request?
 |
 +----> CDI device exists?
 |
 v
ContainerAllocateResponse
 |
 | CdiDevices:
 | nvidia.com/gpu=<UUID>
 |
 v
Container Runtime
 |
 v
NVIDIA CDI
 |
 v
Container
 |
 v
Physical GPU
```

`Allocate()` doesn't blindly trust the IDs sent in the request.

Each requested UUID is validated before the plugin returns a CDI device.

---

# CDI Integration

The plugin uses the Container Device Interface (CDI) to expose GPUs to containers.

It does **not** manually inject:

```text
/dev/nvidia0
/dev/nvidiactl
/dev/nvidia-uvm
```

and it doesn't manually construct NVIDIA-specific env vars or mounts.

Instead, it returns a CDI device:

```text
nvidia.com/gpu=<GPU-UUID>
```

Example:

```text
nvidia.com/gpu=GPU-c0a1bd7d-9471-1ea3-0e5d-fbaead894758
```

The container runtime and NVIDIA CDI spec handle the actual device injection.

Before a GPU is advertised/allocated, the plugin validates that the corresponding CDI device exists.

Check available CDI devices with:

```bash
nvidia-ctk cdi list
```

If NVML sees a GPU but CDI doesn't contain the matching UUID, the device won't be considered usable by the plugin.

---

# Health Monitoring

GPU health is tracked through NVML.

Kubernetes Device Plugin health values are:

```text
Healthy
Unhealthy
```

The plugin uses two mechanisms:

1. NVML XID critical error events
2. periodic NVML probes

## XID events

If NVML reports a critical XID event for a GPU, the plugin marks it:

```text
Unhealthy
```

The state change is propagated through `ListAndWatch`.

Conceptually:

```text
NVML XID
   |
   v
HealthUpdate
   |
   v
devicePlugin.setHealth()
   |
   v
updates channel
   |
   v
ListAndWatch
   |
   v
kubelet
```

kubelet can then stop considering the device available for new allocations.

## Recovery

An unhealthy GPU isn't immediately returned to service after one successful probe.

Recovery uses:

```text
GPU_HEALTH_RECOVERY_DELAY
```

and:

```text
GPU_HEALTH_RECOVERY_SUCCESSES
```

With the defaults:

```text
GPU_HEALTH_RECOVERY_DELAY=30s
GPU_HEALTH_RECOVERY_SUCCESSES=3
```

the GPU needs to wait at least 30 seconds and pass three successful probes before being marked `Healthy` again.

This helps avoid rapid health-state flapping.

---

# kubelet Restart Handling

kubelet exposes its Device Plugin registration socket at:

```text
/var/lib/kubelet/device-plugins/kubelet.sock
```

When kubelet restarts, that socket may be removed and recreated.

The plugin watches:

```text
/var/lib/kubelet/device-plugins
```

using filesystem notifications.

When a new `kubelet.sock` is detected, the manager re-registers all active GPU resources.

Flow:

```text
kubelet restart
      |
      v
old kubelet.sock removed
      |
      v
new kubelet.sock created
      |
      v
fsnotify event
      |
      v
Manager
      |
      v
reregisterAll()
      |
      v
all GPU resources registered again
```

A normal kubelet restart therefore shouldn't require manually restarting the GPU plugin.

---

# Plugin Socket Recovery

Each resource owns a Unix socket.

Example:

```text
/var/lib/kubelet/device-plugins/k8s-gpu-rtx-4090.sock
```

The manager watches the Device Plugin directory for socket removal or rename events.

If a plugin socket disappears, the manager attempts to:

```text
stop old endpoint
      |
      v
remove stale state
      |
      v
create Unix socket
      |
      v
start gRPC server
      |
      v
register with kubelet
```

This prevents a deleted endpoint from silently leaving the resource unavailable.

---

# Duplicate GPU Protection

A physical GPU is identified by its NVML UUID.

Example:

```text
GPU-c0a1bd7d-9471-1ea3-0e5d-fbaead894758
```

The same UUID must not be advertised under multiple resources.

Invalid state:

```text
gpu.local/rtx-4090
└── GPU-A

gpu.local/some-other-resource
└── GPU-A
```

The manager checks this during startup.

If the same UUID would be mapped to multiple plugins, startup fails instead of double-advertising the physical device.

---

# Graceful Shutdown

The process handles:

```text
SIGINT
SIGTERM
```

This matters when running under:

- systemd
- Kubernetes
- containerd
- Docker
- a process supervisor

On shutdown:

```text
signal
  |
  v
context canceled
  |
  v
Manager.Run() exits
  |
  v
plugins stop
  |
  +----> gRPC servers stop
  |
  +----> listeners close
  |
  +----> plugin sockets removed
  |
  v
NVML shutdown
```

This avoids leaving stale Device Plugin sockets behind after a normal shutdown.

---

# Project Structure

```text
k8s-gpu-device-plugin/
├── cdi.go
├── go.mod
├── go.sum
├── gpu.go
├── LICENSE
├── main.go
├── manager.go
├── plugin.go
└── docs.md
```

## `main.go`

Application entry point and config.

Responsible for:

```text
Config
env parsing
NVML init/shutdown
signal handling
dependency wiring
Manager startup
```

## `gpu.go`

GPU/NVML layer.

Responsible for:

```text
GPU model
GPU discovery
model normalization
resource naming
NVML health monitoring
health recovery
```

## `cdi.go`

CDI abstraction.

Responsible for validating:

```text
nvidia.com/gpu=<UUID>
```

against the host CDI cache.

## `plugin.go`

Kubernetes Device Plugin implementation.

Responsible for:

```text
gRPC server
kubelet registration
ListAndWatch
Allocate
health state
plugin socket lifecycle
```

## `manager.go`

Top-level lifecycle coordinator.

Responsible for:

```text
GPU grouping
plugin creation
startup rollback
UUID ownership
health routing
kubelet restart handling
plugin socket recovery
shutdown
```

---

# Operational Checks

## GPU visibility

```bash
nvidia-smi -L
```

## NVML/driver status

```bash
nvidia-smi
```

## CDI devices

```bash
nvidia-ctk cdi list
```

## Device Plugin sockets

```bash
ls -lah /var/lib/kubelet/device-plugins/
```

Example:

```text
kubelet.sock
k8s-gpu-rtx-3060-ti.sock
k8s-gpu-gtx-1660-super.sock
```

## K8s node resources

```bash
kubectl describe node <node-name>
```

Or:

```bash
kubectl get node <node-name> -o json | jq '.status.capacity'
```

And:

```bash
kubectl get node <node-name> -o json | jq '.status.allocatable'
```

## kubelet logs

```bash
journalctl -u kubelet -f
```

## Plugin process

```bash
ps aux | grep k8s-gpu-device-plugin
```

---

# Troubleshooting

## `no supported NVIDIA GPUs discovered`

Check whether the host driver sees the GPU:

```bash
nvidia-smi -L
```

If `nvidia-smi` can't see the GPU, the plugin won't be able to discover it through NVML either.

---

## `no GPUs have usable CDI devices`

NVML discovered one or more GPUs, but the matching CDI devices weren't available.

Check:

```bash
nvidia-ctk cdi list
```

The GPU UUID discovered through:

```bash
nvidia-smi -L
```

should have a corresponding entry:

```text
nvidia.com/gpu=<UUID>
```

---

## `CDI device "nvidia.com/gpu=..." not found`

The host CDI config doesn't contain the requested physical GPU.

Check:

```bash
nvidia-ctk cdi list
```

Also verify the NVIDIA Container Toolkit installation/config.

---

## `kubelet.sock does not exist`

Check:

```bash
ls -l /var/lib/kubelet/device-plugins/kubelet.sock
```

Then:

```bash
systemctl status kubelet
```

and:

```bash
journalctl -u kubelet -n 100
```

---

## Resource doesn't show up on the node

First verify the plugin process is running.

Then check its socket:

```bash
ls -lah /var/lib/kubelet/device-plugins/
```

Check kubelet logs:

```bash
journalctl -u kubelet -f
```

Then inspect node resources:

```bash
kubectl get node <node-name> -o json | jq '.status.capacity, .status.allocatable'
```

---

## Pod stays `Pending`

Inspect the Pod:

```bash
kubectl describe pod <pod-name>
```

Typical causes include:

- requested GPU model isn't available on any schedulable node
- requested GPU count exceeds `Allocatable`
- node selector/affinity prevents placement
- taints/tolerations prevent placement
- GPU is currently allocated
- GPU was marked `Unhealthy`

Check:

```bash
kubectl get nodes
kubectl describe node <node-name>
kubectl describe pod <pod-name>
```

---

## Pod starts but doesn't see the GPU

Check CDI first:

```bash
nvidia-ctk cdi list
```

Then test the runtime outside the workload if necessary.

Inside the Pod:

```bash
nvidia-smi -L
```

The selected GPU should be visible.

---

# Known Limitations

The current implementation has several intentional limitations.

## NVIDIA only

GPU discovery and health monitoring are based on NVML.

AMD and Intel GPUs aren't supported by the current backend.

The internal interfaces make it possible to add other discovery backends later, but they aren't implemented today.

## Startup-time discovery

Physical GPU discovery currently happens during plugin startup.

Runtime GPU hot-add/hot-remove doesn't trigger a full topology rediscovery.

If the physical GPU topology changes, restart the plugin.

## XID recovery policy

The current health recovery logic uses successful NVML probes after a recovery delay.

Not every NVIDIA XID should necessarily be treated as automatically recoverable in a strict production environment.

For production clusters with aggressive fault isolation, XID errors should be classified and some classes should remain `Unhealthy` until:

```text
GPU reset
node reboot
operator intervention
```

## No topology-aware allocation

The plugin currently doesn't implement topology-aware preferred allocation based on:

- NUMA
- PCIe topology
- NVLink
- NVSwitch

`GetPreferredAllocationAvailable` is reported as `false`.

## No PreStart requirement

The plugin doesn't require a `PreStartContainer` operation.

`PreStartRequired` is reported as `false`.

---

# Security Notes

The plugin runs on GPU nodes and interacts with host-level components.

In production, avoid granting permissions beyond what's required.

The process needs access to:

```text
/var/lib/kubelet/device-plugins
```

and the host NVIDIA/NVML stack.

If deployed as a DaemonSet, host mounts and container privileges should be kept as narrow as possible.

Don't expose the Device Plugin socket directory to unrelated workloads.

The Kubernetes Device Plugin API is a node-local control-plane interface and should be treated accordingly.

---

# Production Deployment

For production use, the plugin should normally run as a DaemonSet on GPU nodes rather than as a manually launched binary.

Typical deployment model:

```text
K8s Cluster
    |
    +---- CPU Node
    |
    +---- CPU Node
    |
    +---- GPU Node
    |       |
    |       └── k8s-gpu-device-plugin Pod
    |
    +---- GPU Node
            |
            └── k8s-gpu-device-plugin Pod
```

The DaemonSet should only target NVIDIA GPU nodes.

Typical mechanisms include:

```text
nodeSelector
nodeAffinity
taints/tolerations
```

The plugin container needs the host Device Plugin directory mounted:

```text
/var/lib/kubelet/device-plugins
```

It also needs access to the NVIDIA driver/NVML environment required by the container runtime.

Before rolling it out cluster-wide, validate the binary directly on a single GPU node.

Recommended rollout:

```text
1. Build
2. Run go vet
3. Run tests
4. Test on one GPU node
5. Verify CDI
6. Verify Capacity/Allocatable
7. Run a single-GPU Pod
8. Run a multi-GPU Pod
9. Restart kubelet and verify re-registration
10. Deploy as a DaemonSet
```

---

# Quick Start

## 1. Verify the GPU

```bash
nvidia-smi -L
```

## 2. Verify CDI

```bash
nvidia-ctk cdi list
```

## 3. Build

```bash
go mod tidy
go vet ./...
go test ./...
go build -o k8s-gpu-device-plugin .
```

## 4. Run

```bash
sudo GPU_RESOURCE_DOMAIN=gpu.local ./k8s-gpu-device-plugin
```

## 5. Verify K8s resources

```bash
kubectl get node <node-name> -o json | jq '.status.allocatable'
```

Example:

```json
{
  "gpu.local/gtx-1660-super": "1",
  "gpu.local/rtx-3060-ti": "1"
}
```

## 6. Request a GPU

```yaml
apiVersion: v1
kind: Pod

metadata:
  name: gpu-test

spec:
  restartPolicy: Never

  containers:
    - name: cuda
      image: nvidia/cuda:12.1.0-base-ubuntu22.04
      command: ["bash", "-c", "nvidia-smi -L"]

      resources:
        limits:
          gpu.local/rtx-3060-ti: 1
```

Apply:

```bash
kubectl apply -f pod.yaml
```

Check:

```bash
kubectl logs gpu-test
```

The workload should see the GPU model it requested.

---

# Design Summary

The plugin intentionally keeps GPU identity and K8s resource identity separate.

A physical device is identified by its immutable GPU UUID:

```text
GPU-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
```

The scheduler-facing resource represents a GPU model:

```text
gpu.local/rtx-4090
```

CDI bridges the physical GPU into the container:

```text
nvidia.com/gpu=<UUID>
```

So the complete mapping is:

```text
GPU Model
   |
   v
K8s Extended Resource
   |
   v
kubelet selects Device ID
   |
   v
Physical GPU UUID
   |
   v
NVIDIA CDI Device
   |
   v
Container
```

This gives K8s model-aware GPU scheduling while preserving UUID-level allocation and isolation.