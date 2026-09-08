# k8s-gpu-device-plugin

`k8s-gpu-device-plugin` is a Kubernetes Device Plugin for NVIDIA GPUs that exposes physical GPUs as **model-specific Kubernetes extended resources** and allocates the selected devices to containers through CDI.

Instead of advertising every NVIDIA GPU through a single generic resource:

```text
nvidia.com/gpu
```

the plugin discovers the actual GPU models installed on each node and exposes resources such as:

```text
gpu.local/rtx-3060-ti
gpu.local/gtx-1660-super
gpu.local/rtx-4090
gpu.local/a100-sxm4-40gb
```

A workload can therefore request a specific GPU model using the standard Kubernetes resource mechanism:

```yaml
resources:
  limits:
    gpu.local/rtx-3060-ti: 1
```

The plugin handles:

- NVML-based GPU discovery
- GPU UUID tracking
- model normalization
- model-specific resource registration
- kubelet Device Plugin registration
- CDI-backed device allocation
- GPU health monitoring
- `ListAndWatch` updates
- kubelet re-registration
- plugin socket recovery
- graceful shutdown

The intended deployment model is a **DaemonSet running on GPU nodes**.

---

# Table of Contents

- [Overview](#overview)
- [Why This Exists](#why-this-exists)
- [Architecture](#architecture)
- [Features](#features)
- [Requirements](#requirements)
- [Repository Layout](#repository-layout)
- [Configuration](#configuration)
- [Building From Source](#building-from-source)
- [Container Image](#container-image)
- [CI and GHCR](#ci-and-ghcr)
- [Kubernetes Deployment](#kubernetes-deployment)
- [GPU Discovery](#gpu-discovery)
- [Resource Naming](#resource-naming)
- [Device Plugin Registration](#device-plugin-registration)
- [Requesting a GPU](#requesting-a-gpu)
- [Allocation Flow](#allocation-flow)
- [Multi-GPU Allocation](#multi-gpu-allocation)
- [CDI Integration](#cdi-integration)
- [Health Monitoring](#health-monitoring)
- [kubelet Restart Handling](#kubelet-restart-handling)
- [Plugin Socket Recovery](#plugin-socket-recovery)
- [Duplicate GPU Protection](#duplicate-gpu-protection)
- [Graceful Shutdown](#graceful-shutdown)
- [Operational Checks](#operational-checks)
- [Troubleshooting](#troubleshooting)
- [Known Limitations](#known-limitations)
- [Deployment Considerations](#deployment-considerations)
- [End-to-End Validation](#end-to-end-validation)
- [Design Summary](#design-summary)

---

# Overview

The plugin runs on a Kubernetes GPU node and communicates with three node-local systems:

```text
NVIDIA Driver / NVML
        |
        v
k8s-gpu-device-plugin
        |
        +------> kubelet Device Plugin API
        |
        +------> CDI device specifications
```

At startup, the plugin uses NVML to enumerate physical NVIDIA GPUs.

For every GPU it obtains:

```text
GPU UUID
GPU model
Kubernetes resource name
CDI device name
health state
```

For example, a node containing:

```text
GPU 0: NVIDIA GeForce RTX 3060 Ti
GPU 1: NVIDIA GeForce RTX 3060 Ti
GPU 2: NVIDIA GeForce GTX 1660 SUPER
```

is represented internally as:

```text
gpu.local/rtx-3060-ti
├── GPU-UUID-A
└── GPU-UUID-B

gpu.local/gtx-1660-super
└── GPU-UUID-C
```

Kubernetes sees:

```text
gpu.local/rtx-3060-ti:    2
gpu.local/gtx-1660-super: 1
```

The core design invariant is:

```text
1 Device Plugin endpoint
=
1 Kubernetes resource name
=
1..N physical GPUs of the same model
```

A physical GPU is identified by its NVML UUID and must not be advertised by multiple plugin resources.

---

# Why This Exists

The conventional NVIDIA resource:

```text
nvidia.com/gpu
```

represents GPU quantity but doesn't inherently express the exact GPU model requested by a workload.

That can be limiting in heterogeneous environments.

Consider a cluster containing:

```text
Node A
└── RTX 4090

Node B
├── RTX 3060 Ti
└── RTX 3060 Ti

Node C
└── GTX 1660 SUPER
```

A generic request:

```yaml
resources:
  limits:
    nvidia.com/gpu: 1
```

doesn't describe whether the workload requires an RTX 4090, RTX 3060 Ti, or another GPU model.

With this plugin, nodes can advertise:

```text
gpu.local/rtx-4090:       1
gpu.local/rtx-3060-ti:    2
gpu.local/gtx-1660-super: 1
```

and a workload can explicitly request:

```yaml
resources:
  limits:
    gpu.local/rtx-4090: 1
```

Kubernetes can then schedule the workload only onto a node that advertises the requested model.

This is useful when GPU model, VRAM capacity, architecture, or workload placement matters.

---

# Architecture

The plugin is a **node agent**, not a centralized GPU controller.

A Kubernetes cluster should conceptually look like:

```text
                       Kubernetes Control Plane
                                |
                                |
                +---------------+---------------+
                |                               |
                v                               v

          GPU Node A                       GPU Node B

       NVIDIA Driver                    NVIDIA Driver
             |                                |
            NVML                             NVML
             |                                |
             v                                v

 k8s-gpu-device-plugin            k8s-gpu-device-plugin
             |                                |
             v                                v

          kubelet                          kubelet
             |                                |
             +---------------+----------------+
                             |
                             v
                       Kubernetes API
```

Each plugin instance only manages GPUs physically attached to its own node.

The plugin talks to the local kubelet through:

```text
/var/lib/kubelet/device-plugins/kubelet.sock
```

The Kubernetes scheduler doesn't communicate with the plugin directly. It schedules against the extended resources reported by kubelet.

---

## Internal Architecture

```text
                    +------------------+
                    |      Config      |
                    +--------+---------+
                             |
                             v
                    +------------------+
                    |       NVML       |
                    +--------+---------+
                             |
                             v
                    +------------------+
                    |   GPU Discovery  |
                    +--------+---------+
                             |
                             v
                    +------------------+
                    |      Manager     |
                    +--------+---------+
                             |
               +-------------+-------------+
               |                           |
               v                           v

     gpu.local/rtx-3060-ti      gpu.local/gtx-1660-super

          Device Plugin               Device Plugin
          gRPC endpoint               gRPC endpoint

               |                           |
               +-------------+-------------+
                             |
                             v
                          kubelet
                             |
                             v
                         Allocate()
                             |
                             v
                            CDI
                             |
                             v
                    Container Runtime
                             |
                             v
                         Workload
```

The `Manager` coordinates the node-level lifecycle:

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
Create Device Plugin endpoints
    |
    v
Register resources with kubelet
    |
    +------> Monitor GPU health
    |
    +------> Watch kubelet.sock
    |
    +------> Watch plugin sockets
    |
    v
Graceful shutdown
```

---

# Features

Current functionality includes:

- automatic NVIDIA GPU discovery through NVML
- no hardcoded GPU UUIDs
- automatic model-to-resource conversion
- configurable Kubernetes resource domain
- one Device Plugin endpoint per GPU model
- multiple physical GPUs under one model resource
- multi-GPU allocation
- UUID validation during allocation
- resource ownership validation
- duplicate device request detection
- GPU health validation
- CDI device validation
- CDI-backed GPU injection
- NVML XID critical error monitoring
- periodic NVML health probes
- delayed health recovery
- `ListAndWatch` state propagation
- kubelet socket monitoring
- automatic kubelet re-registration
- plugin socket recovery
- duplicate UUID protection
- startup rollback
- SIGINT/SIGTERM handling
- graceful gRPC shutdown
- discovery, registration, and CDI abstractions for testability
- container image build
- GitHub Actions CI
- GHCR image publishing
- Kubernetes DaemonSet deployment

---

# Requirements

A target GPU node needs:

- Linux
- Kubernetes with kubelet
- NVIDIA GPU
- NVIDIA driver
- NVML
- NVIDIA Container Toolkit / CDI setup
- a CDI-capable container runtime
- access to the kubelet Device Plugin directory

The default Device Plugin directory is:

```text
/var/lib/kubelet/device-plugins
```

## NVIDIA driver

Verify the driver:

```bash
nvidia-smi
```

List physical GPUs:

```bash
nvidia-smi -L
```

Example:

```text
GPU 0: NVIDIA GeForce RTX 3060 Ti (UUID: GPU-...)
GPU 1: NVIDIA GeForce GTX 1660 SUPER (UUID: GPU-...)
```

## CDI

List CDI devices:

```bash
nvidia-ctk cdi list
```

The plugin expects physical GPUs to have CDI names in the form:

```text
nvidia.com/gpu=<GPU-UUID>
```

For example:

```text
nvidia.com/gpu=GPU-c0a1bd7d-9471-1ea3-0e5d-fbaead894758
```

If NVML discovers a GPU but the corresponding CDI device doesn't exist, the GPU isn't advertised by the plugin.

---

# Repository Layout

The repository is organized as:

```text
k8s-gpu-device-plugin/
├── .github/
│   └── workflows/
│       └── go.yaml
│
├── deploy/
│   └── daemonset.yaml
│
├── docs/
│   ├── LICENSE
│   ├── README.md
│   └── docs.md
│
├── src/
│   ├── cdi.go
│   ├── go.mod
│   ├── go.sum
│   ├── gpu.go
│   ├── main.go
│   ├── manager.go
│   └── plugin.go
│
├── Dockerfile
└── .gitignore
```

## `src/main.go`

Application entry point and configuration.

Responsibilities:

```text
environment configuration
NVML initialization
NVML shutdown
signal handling
dependency wiring
Manager startup
```

## `src/gpu.go`

GPU and NVML layer.

Responsibilities:

```text
GPU representation
NVML discovery
model normalization
resource naming
XID monitoring
periodic health probes
health recovery
```

## `src/cdi.go`

CDI abstraction.

Validates that a CDI device such as:

```text
nvidia.com/gpu=<UUID>
```

exists before the physical GPU is advertised or allocated.

## `src/plugin.go`

Kubernetes Device Plugin implementation.

Responsibilities:

```text
gRPC server
kubelet registration
GetDevicePluginOptions
ListAndWatch
Allocate
health state
plugin socket lifecycle
```

## `src/manager.go`

Top-level node lifecycle coordinator.

Responsibilities:

```text
GPU discovery orchestration
CDI validation
resource grouping
plugin creation
startup rollback
UUID ownership
health update routing
kubelet restart handling
plugin socket recovery
shutdown
```

## `deploy/daemonset.yaml`

Kubernetes DaemonSet used to run one plugin Pod on each targeted node.

## `.github/workflows/go.yaml`

CI pipeline for:

```text
dependency verification
go vet
go test
Go binary build
container image build
GHCR publishing
```

---

# Configuration

Configuration is provided through environment variables.

| Environment Variable | Default | Description |
|---|---:|---|
| `GPU_RESOURCE_DOMAIN` | `gpu.local` | Domain used for Kubernetes extended resources |
| `GPU_PLUGIN_PATH` | `/var/lib/kubelet/device-plugins` | kubelet Device Plugin directory |
| `GPU_HEALTH_PROBE_INTERVAL` | `5s` | Interval between NVML health probes |
| `GPU_HEALTH_RECOVERY_DELAY` | `30s` | Minimum time before recovery can begin |
| `GPU_HEALTH_RECOVERY_SUCCESSES` | `3` | Consecutive successful probes required for recovery |

Example:

```bash
GPU_RESOURCE_DOMAIN=gpu.example.com \
GPU_HEALTH_PROBE_INTERVAL=5s \
GPU_HEALTH_RECOVERY_DELAY=30s \
GPU_HEALTH_RECOVERY_SUCCESSES=3 \
./k8s-gpu-device-plugin
```

## Resource Domain

For local development:

```text
gpu.local
```

is sufficient.

For a real shared cluster, use a domain controlled by the organization:

```text
gpu.example.com
```

This produces resources such as:

```text
gpu.example.com/rtx-4090
gpu.example.com/rtx-3060-ti
gpu.example.com/a100-sxm4-40gb
```

---

# Building From Source

The Go module is located under:

```text
src/
```

Clone the repository and enter the source directory:

```bash
git clone https://github.com/maxagateff/k8s-gpu-device-plugin.git
cd k8s-gpu-device-plugin/src
```

For the current development branch:

```bash
git checkout develop
```

Verify dependencies:

```bash
go mod verify
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

A normal local validation cycle is:

```bash
go fmt ./...
go mod tidy
go mod verify
go vet ./...
go test ./...
go build -o k8s-gpu-device-plugin .
```

Because the project uses NVIDIA's Go NVML bindings, the container build enables CGO.

---

# Container Image

The repository contains a multi-stage Docker build.

Conceptually:

```text
golang:1.24
    |
    | CGO_ENABLED=1
    | go build
    v
k8s-gpu-device-plugin binary
    |
    v
ubuntu:24.04 runtime image
```

Build locally from the repository root:

```bash
docker build -t k8s-gpu-device-plugin:dev .
```

The resulting container starts:

```text
/usr/local/bin/k8s-gpu-device-plugin
```

The image intentionally doesn't contain an NVIDIA kernel driver.

The plugin needs access to the NVIDIA driver/NVML environment of the GPU node at runtime.

---

# CI and GHCR

GitHub Actions is configured in:

```text
.github/workflows/go.yaml
```

Pushes to the configured development/release branches run the CI pipeline.

The pipeline performs:

```text
checkout
   |
   v
go mod verify
   |
   v
go vet ./...
   |
   v
go test ./...
   |
   v
go build
   |
   v
docker build
   |
   v
GHCR push
```

The container image is published to:

```text
ghcr.io/maxagateff/k8s-gpu-device-plugin
```

The workflow publishes:

```text
ghcr.io/maxagateff/k8s-gpu-device-plugin:latest
```

and an immutable image tag based on the Git commit SHA.

This gives two useful references:

```text
latest
```

for development/testing, and:

```text
<commit-sha>
```

for reproducible deployments.

For production deployments, prefer an immutable version or commit tag rather than relying permanently on `latest`.

---

# Kubernetes Deployment

The intended deployment model is a Kubernetes `DaemonSet`.

Why a DaemonSet?

A Device Plugin isn't a central cluster service. It has to run on the node whose hardware it manages.

```text
Kubernetes Cluster

GPU Node A
├── RTX 3060 Ti
├── kubelet
└── k8s-gpu-device-plugin Pod

GPU Node B
├── A100
├── kubelet
└── k8s-gpu-device-plugin Pod
```

Each instance:

1. discovers local GPUs through NVML
2. validates local CDI devices
3. connects to the local kubelet
4. registers local model-specific resources

The control plane then sees the resources reported by every node.

Deploy:

```bash
kubectl apply -f deploy/daemonset.yaml
```

Check the DaemonSet:

```bash
kubectl get daemonset -A | grep k8s-gpu-device-plugin
```

Check plugin Pods:

```bash
kubectl get pods -A -l app=k8s-gpu-device-plugin -o wide
```

Read logs:

```bash
kubectl logs -n <namespace> -l app=k8s-gpu-device-plugin
```

Remove the deployment:

```bash
kubectl delete -f deploy/daemonset.yaml
```

---

## Required Host Access

The plugin needs access to the node's Device Plugin directory:

```text
/var/lib/kubelet/device-plugins
```

The DaemonSet mounts this path into the container using `hostPath`.

This makes the node's:

```text
/var/lib/kubelet/device-plugins/kubelet.sock
```

available to the plugin process inside the container.

The deployment also exposes the host CDI directories:

```text
/etc/cdi
/var/run/cdi
```

so the plugin can validate the CDI devices generated for the node.

The current deployment also makes the node's NVML library available to the container:

```text
/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1
```

Without NVML access, startup fails with:

```text
FATAL: initialize NVML: ERROR_LIBRARY_NOT_FOUND
```

---

# GPU Discovery

GPU discovery happens through NVML.

Conceptually:

```text
nvml.DeviceGetCount()
        |
        v
nvml.DeviceGetHandleByIndex(i)
        |
        +------> GetUUID()
        |
        +------> GetName()
```

A discovered GPU is represented internally by:

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
Model:
NVIDIA GeForce RTX 3060 Ti

UUID:
GPU-c0a1bd7d-9471-1ea3-0e5d-fbaead894758

Resource:
gpu.local/rtx-3060-ti

CDI:
nvidia.com/gpu=GPU-c0a1bd7d-9471-1ea3-0e5d-fbaead894758

Health:
Healthy
```

Duplicate UUIDs discovered through NVML cause startup to fail rather than allowing ambiguous physical device identity.

---

# Resource Naming

GPU model names are normalized automatically.

For example:

```text
NVIDIA GeForce RTX 3060 Ti
```

becomes:

```text
rtx-3060-ti
```

and with:

```text
GPU_RESOURCE_DOMAIN=gpu.local
```

the final resource is:

```text
gpu.local/rtx-3060-ti
```

Additional examples:

```text
NVIDIA GeForce GTX 1660 SUPER
-> gpu.local/gtx-1660-super

NVIDIA GeForce RTX 4090
-> gpu.local/rtx-4090

NVIDIA A100-SXM4-40GB
-> gpu.local/a100-sxm4-40gb
```

The `NVIDIA` and `NVIDIA GeForce` prefixes are removed during canonicalization.

Other separators are normalized into `-`.

Resource suffixes are validated before registration.

---

# Device Plugin Registration

Each model-specific resource receives its own Device Plugin Unix socket.

Example:

```text
gpu.local/rtx-3060-ti
```

uses:

```text
/var/lib/kubelet/device-plugins/k8s-gpu-rtx-3060-ti.sock
```

while:

```text
gpu.local/gtx-1660-super
```

uses:

```text
/var/lib/kubelet/device-plugins/k8s-gpu-gtx-1660-super.sock
```

Each endpoint is registered independently with kubelet.

Successful startup logs look similar to:

```text
k8s-gpu-device-plugin starting domain=gpu.local pluginPath=/var/lib/kubelet/device-plugins

discovered GPU model="NVIDIA GeForce RTX 3060 Ti" uuid=GPU-... resource=gpu.local/rtx-3060-ti cdi=nvidia.com/gpu=GPU-...

registered resource=gpu.local/rtx-3060-ti socket=/var/lib/kubelet/device-plugins/k8s-gpu-rtx-3060-ti.sock devices=1
```

After registration:

```bash
kubectl get node <node-name> -o json | jq '.status.capacity, .status.allocatable'
```

should contain entries such as:

```json
{
  "gpu.local/gtx-1660-super": "1",
  "gpu.local/rtx-3060-ti": "1"
}
```

---

# Requesting a GPU

A workload requests a model using a normal Kubernetes resource limit.

Example:

```yaml
apiVersion: v1
kind: Pod

metadata:
  name: test-rtx
  namespace: test-code

spec:
  restartPolicy: Never

  containers:
    - name: test
      image: nvidia/cuda:12.1.0-base-ubuntu22.04
      command: ["bash", "-c", "nvidia-smi -L"]

      resources:
        limits:
          gpu.local/rtx-3060-ti: 1
```

Apply:

```bash
kubectl apply -f test-gpu-test-pod.yaml
```

Check:

```bash
kubectl get pod -n test-code test-rtx -o wide
```

Read the result:

```bash
kubectl logs -n test-code test-rtx
```

A successful allocation looks like:

```text
GPU 0: NVIDIA GeForce RTX 3060 Ti (UUID: GPU-...)
```

The workload receives the physical GPU selected by kubelet and returned through CDI by the plugin.

---

# Allocation Flow

The complete scheduling and allocation path is:

```text
Pod
 |
 | requests:
 | gpu.local/rtx-3060-ti: 1
 |
 v
Kubernetes Scheduler
 |
 | selects node with allocatable resource
 |
 v
kubelet
 |
 | selects Device ID / GPU UUID
 |
 v
k8s-gpu-device-plugin
 |
 | Allocate()
 |
 +------> UUID exists?
 |
 +------> UUID belongs to this resource?
 |
 +------> duplicate request?
 |
 +------> GPU Healthy?
 |
 +------> CDI device exists?
 |
 v
ContainerAllocateResponse
 |
 | CDI device:
 | nvidia.com/gpu=<GPU-UUID>
 |
 v
Container Runtime
 |
 v
CDI
 |
 v
Physical NVIDIA GPU
 |
 v
Workload Container
```

The plugin doesn't blindly trust the device IDs in an allocation request.

Every requested UUID is validated before a CDI device is returned.

---

# Multi-GPU Allocation

Multiple physical GPUs of the same model are grouped under one resource.

Example:

```text
GPU-A -> RTX 4090
GPU-B -> RTX 4090
GPU-C -> RTX 4090
GPU-D -> RTX 4090
```

The node advertises:

```text
gpu.local/rtx-4090: 4
```

A workload can request:

```yaml
resources:
  limits:
    gpu.local/rtx-4090: 2
```

kubelet can then select two device IDs:

```text
GPU-A
GPU-C
```

The plugin validates both and returns:

```text
nvidia.com/gpu=GPU-A
nvidia.com/gpu=GPU-C
```

through the Device Plugin API's CDI response.

---

# CDI Integration

The plugin uses the Container Device Interface instead of manually constructing NVIDIA device mounts.

It doesn't manually inject:

```text
/dev/nvidia0
/dev/nvidiactl
/dev/nvidia-uvm
```

and doesn't manually construct NVIDIA-specific environment variables.

Instead, allocation returns:

```text
nvidia.com/gpu=<GPU-UUID>
```

The container runtime and NVIDIA-generated CDI specification perform the actual device injection.

This keeps the plugin focused on:

```text
discovery
resource identity
device selection validation
health
kubelet integration
```

while CDI handles container-level device injection.

Before advertising a GPU, the manager checks that its CDI device exists.

The CDI cache is refreshed during lookup.

---

# Health Monitoring

GPU health is monitored through NVML.

Kubernetes Device Plugin health states are:

```text
Healthy
Unhealthy
```

The plugin uses:

1. NVML XID critical error events
2. periodic NVML probes

---

## XID Monitoring

The health monitor registers for:

```text
nvml.EventTypeXidCriticalError
```

When a critical XID event is received:

```text
NVML XID event
      |
      v
HealthUpdate
      |
      v
devicePlugin.setHealth()
      |
      v
ListAndWatch update
      |
      v
kubelet
```

The affected GPU is marked:

```text
Unhealthy
```

and kubelet receives the new state through `ListAndWatch`.

---

## Periodic Probes

The plugin periodically resolves each known GPU through NVML.

The default probe interval is:

```text
5s
```

A failed probe marks the GPU unhealthy.

---

## Recovery

An unhealthy GPU isn't immediately restored after one successful probe.

The defaults are:

```text
GPU_HEALTH_RECOVERY_DELAY=30s
GPU_HEALTH_RECOVERY_SUCCESSES=3
```

After the recovery delay, the GPU must pass the configured number of successful probes before being marked:

```text
Healthy
```

again.

This reduces health-state flapping.

---

# kubelet Restart Handling

The kubelet Device Plugin registration socket is:

```text
/var/lib/kubelet/device-plugins/kubelet.sock
```

When kubelet restarts, this socket can be recreated.

The manager watches the Device Plugin directory using `fsnotify`.

When creation of the kubelet socket is detected:

```text
kubelet restart
      |
      v
kubelet.sock recreated
      |
      v
fsnotify
      |
      v
Manager.reregisterAll()
      |
      v
all active resources registered again
```

This is designed to allow kubelet restarts without manually restarting the GPU plugin.

---

# Plugin Socket Recovery

Each resource has its own Unix socket.

For example:

```text
/var/lib/kubelet/device-plugins/k8s-gpu-rtx-3060-ti.sock
```

The manager watches for removal or rename events affecting these sockets.

If one disappears:

```text
plugin socket removed
      |
      v
stop old endpoint
      |
      v
recreate Unix socket
      |
      v
restart gRPC server
      |
      v
register resource with kubelet
```

This prevents accidental socket deletion from permanently removing the resource endpoint while the process is still running.

---

# Duplicate GPU Protection

Physical identity is based on the NVIDIA GPU UUID.

Example:

```text
GPU-c0a1bd7d-9471-1ea3-0e5d-fbaead894758
```

A UUID must belong to exactly one model-specific resource.

Invalid state:

```text
gpu.local/rtx-4090
└── GPU-A

gpu.local/some-other-resource
└── GPU-A
```

Duplicate protection exists during discovery and resource/plugin construction.

The plugin fails rather than knowingly advertising one physical GPU through multiple internal resource groups.

---

# Graceful Shutdown

The process handles:

```text
SIGINT
SIGTERM
```

Shutdown flow:

```text
SIGTERM / SIGINT
       |
       v
context canceled
       |
       v
Manager exits
       |
       v
Device Plugins stop
       |
       +------> gRPC servers stop
       |
       +------> listeners close
       |
       +------> sockets removed
       |
       v
NVML shutdown
```

This is important when the plugin runs as a Kubernetes Pod because Pod termination normally delivers `SIGTERM`.

---

# Operational Checks

## Physical GPUs

```bash
nvidia-smi -L
```

## Driver

```bash
nvidia-smi
```

## NVML library

```bash
ldconfig -p | grep libnvidia-ml
```

## CDI

```bash
nvidia-ctk cdi list
```

## Device Plugin sockets

```bash
ls -lah /var/lib/kubelet/device-plugins/
```

Expected sockets can include:

```text
kubelet.sock
k8s-gpu-rtx-3060-ti.sock
k8s-gpu-gtx-1660-super.sock
```

## DaemonSet

```bash
kubectl get daemonset -A | grep k8s-gpu-device-plugin
```

## Plugin Pods

```bash
kubectl get pods -A -l app=k8s-gpu-device-plugin -o wide
```

## Plugin logs

```bash
kubectl logs -n <namespace> -l app=k8s-gpu-device-plugin
```

## Kubernetes GPU resources

```bash
kubectl get node <node-name> -o json | jq '.status.capacity, .status.allocatable'
```

## kubelet logs

```bash
journalctl -u kubelet -f
```

---

# Troubleshooting

## `initialize NVML: ERROR_LIBRARY_NOT_FOUND`

The plugin container can't access:

```text
libnvidia-ml.so.1
```

Verify the library on the host:

```bash
ldconfig -p | grep libnvidia-ml
```

The container deployment must make the host NVIDIA/NVML runtime available to the plugin.

The current DaemonSet expects the NVML library at:

```text
/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1
```

This path is host/distribution specific and may need to be changed on other systems.

---

## `no supported NVIDIA GPUs discovered`

Verify:

```bash
nvidia-smi -L
```

If the NVIDIA driver can't see the device, NVML discovery won't see it either.

---

## `no GPUs have usable CDI devices`

NVML discovered GPUs, but none had a matching CDI device.

Compare:

```bash
nvidia-smi -L
```

with:

```bash
nvidia-ctk cdi list
```

Each advertised physical GPU needs:

```text
nvidia.com/gpu=<GPU-UUID>
```

---

## `CDI device "nvidia.com/gpu=..." not found`

Refresh/check the host CDI setup:

```bash
nvidia-ctk cdi list
```

Verify that the exact UUID discovered by NVML appears in the CDI output.

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

## Resource Doesn't Appear on the Node

Check plugin logs first:

```bash
kubectl logs -n <namespace> -l app=k8s-gpu-device-plugin
```

Check sockets:

```bash
ls -lah /var/lib/kubelet/device-plugins/
```

Then inspect:

```bash
kubectl get node <node-name> -o json | jq '.status.capacity, .status.allocatable'
```

---

## Pod Stays `Pending`

Inspect:

```bash
kubectl describe pod -n <namespace> <pod-name>
```

Common causes:

```text
requested GPU model isn't available
requested count exceeds Allocatable
GPU is already allocated
GPU is Unhealthy
node selector/affinity prevents scheduling
taints/tolerations prevent scheduling
```

---

## Pod Starts but Doesn't See the GPU

Check the requested resource:

```yaml
resources:
  limits:
    gpu.local/rtx-3060-ti: 1
```

Check CDI:

```bash
nvidia-ctk cdi list
```

Then inside the workload:

```bash
nvidia-smi -L
```

The container should see the selected physical GPU.

---

# Known Limitations

## NVIDIA Only

The current backend depends on NVML.

AMD and Intel GPUs aren't supported.

The discovery abstraction allows additional backends to be introduced later, but they aren't implemented today.

---

## Startup-Time Discovery

GPU topology is discovered during startup.

Physical GPU hot-add/hot-remove doesn't currently trigger full rediscovery and resource regrouping.

Restart the plugin after changing physical GPU topology.

---

## Host-Specific NVML Mount

The current DaemonSet exposes:

```text
/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1
```

from the host.

That path isn't portable across every Linux distribution, driver installation method, or CPU architecture.

A more portable NVIDIA runtime integration should replace the hardcoded host library path for broader distribution.

---

## amd64 Container Build

The current Docker build explicitly targets:

```text
GOARCH=amd64
```

so the published image is currently intended for x86-64 GPU nodes.

Multi-architecture image publishing isn't implemented yet.

---

## Startup CDI Validation

A GPU without a usable CDI device is skipped during startup.

Dynamic CDI topology changes don't currently cause a full GPU/resource rediscovery.

---

## XID Recovery Policy

The current implementation can return an unhealthy GPU to service after the configured delay and successful probes.

Some XID classes may require stricter handling in production, including:

```text
GPU reset
node reboot
operator intervention
```

A future implementation can classify XIDs instead of treating all critical XID events through the same recovery policy.

---

## No Topology-Aware Preferred Allocation

The plugin doesn't currently select GPUs based on:

```text
NUMA
PCIe topology
NVLink
NVSwitch
```

`GetPreferredAllocationAvailable` is disabled.

---

## No PreStart Requirement

The plugin doesn't require `PreStartContainer`.

`PreStartRequired` is disabled.

---

## Resource Domain Validation

Resource suffixes are validated, but the configured resource domain should still be treated as operator-controlled configuration and set to a valid Kubernetes extended-resource domain.

---

## Official NVIDIA Device Plugin Coexistence

Care is required if another Device Plugin advertises the same physical GPUs under a different Kubernetes resource.

For example:

```text
nvidia.com/gpu
```

and:

```text
gpu.local/rtx-3060-ti
```

are different extended resources from Kubernetes' perspective.

Kubernetes doesn't inherently know that both resource names may represent the same physical GPU.

Running independent plugins that advertise the same hardware can therefore create double-allocation risk.

Use one authoritative allocation model for a physical GPU set unless explicit coordination exists between plugins.

---

# Deployment Considerations

The plugin should normally run as a DaemonSet on NVIDIA GPU nodes.

For a heterogeneous cluster:

```text
Control Plane
     |
     +----------------------------------+
     |                                  |
     v                                  v

GPU Node A                         GPU Node B
RTX 3060 Ti                       A100
     |                                  |
plugin Pod                         plugin Pod
     |                                  |
gpu.local/rtx-3060-ti: 1          gpu.local/a100: 1
```

The scheduler can then place model-specific workloads using the normal extended-resource mechanism.

For larger deployments, consider adding:

```text
nodeSelector / nodeAffinity
taints and tolerations
immutable image tags
resource requests/limits for the plugin Pod
securityContext hardening
portable NVIDIA runtime integration
```

The current DaemonSet uses privileged execution. This is convenient during development but should be reviewed and reduced to the minimum host permissions required before production rollout.

---

# End-to-End Validation

A successful deployment should be validated at several levels.

## 1. Plugin Pod

```bash
kubectl get pods -A -l app=k8s-gpu-device-plugin -o wide
```

## 2. Discovery

Plugin logs should contain entries similar to:

```text
discovered GPU model="NVIDIA GeForce RTX 3060 Ti" uuid=GPU-... resource=gpu.local/rtx-3060-ti cdi=nvidia.com/gpu=GPU-...
```

## 3. kubelet Registration

Logs should contain:

```text
registered resource=gpu.local/rtx-3060-ti socket=/var/lib/kubelet/device-plugins/k8s-gpu-rtx-3060-ti.sock devices=1
```

## 4. Node Capacity

```bash
kubectl get node <node-name> -o json | jq '.status.capacity, .status.allocatable'
```

Expected example:

```text
gpu.local/gtx-1660-super: 1
gpu.local/rtx-3060-ti:    1
```

## 5. Real Workload Allocation

Create a Pod requesting:

```yaml
resources:
  limits:
    gpu.local/rtx-3060-ti: 1
```

and execute:

```bash
nvidia-smi -L
```

inside the workload.

A successful end-to-end allocation produces output such as:

```text
GPU 0: NVIDIA GeForce RTX 3060 Ti (UUID: GPU-...)
```

At that point the following path has been validated:

```text
GitHub
   |
   v
GitHub Actions
   |
   v
Go build + tests
   |
   v
container image
   |
   v
GHCR
   |
   v
Kubernetes DaemonSet
   |
   v
NVML discovery
   |
   v
kubelet registration
   |
   v
extended resource
   |
   v
Pod request
   |
   v
Allocate()
   |
   v
CDI
   |
   v
correct physical GPU inside container
```

---

# Design Summary

The design separates three different identities.

## Scheduler Identity

The GPU model is represented as a Kubernetes extended resource:

```text
gpu.local/rtx-3060-ti
```

This is what workloads request.

## Physical Identity

The actual GPU is identified by its NVIDIA UUID:

```text
GPU-c0a1bd7d-9471-1ea3-0e5d-fbaead894758
```

This is what the Device Plugin tracks and kubelet allocates.

## Container Identity

The selected GPU is passed to the container runtime as a CDI device:

```text
nvidia.com/gpu=GPU-c0a1bd7d-9471-1ea3-0e5d-fbaead894758
```

The complete mapping is therefore:

```text
NVIDIA GPU Model
        |
        v
Kubernetes Extended Resource
        |
        v
Scheduler selects Node
        |
        v
kubelet selects Device ID
        |
        v
Physical GPU UUID
        |
        v
Device Plugin Allocate()
        |
        v
NVIDIA CDI Device
        |
        v
Container Runtime
        |
        v
Workload
```

This provides model-aware Kubernetes GPU scheduling while preserving UUID-level physical device allocation and CDI-based container injection.