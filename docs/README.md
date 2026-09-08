# k8s-gpu-device-plugin

A lightweight Kubernetes Device Plugin for NVIDIA GPUs that exposes physical GPUs as model-specific Kubernetes extended resources and allocates them to containers through CDI.

Instead of exposing every NVIDIA GPU through a single generic resource:

```text
nvidia.com/gpu
```

the plugin automatically discovers the GPU model installed on each node and advertises model-specific resources:

```text
gpu.local/rtx-3060-ti
gpu.local/gtx-1660-super
gpu.local/rtx-4090
gpu.local/a100-sxm4-40gb
```

Workloads can request a specific GPU model using standard Kubernetes resource limits:

```yaml
resources:
  limits:
    gpu.local/rtx-3060-ti: 1
```

## Features

- automatic NVIDIA GPU discovery through NVML
- model-specific Kubernetes extended resources
- UUID-based physical GPU tracking
- automatic GPU model normalization
- multiple physical GPUs per model resource
- multi-GPU allocation
- CDI-backed device injection
- GPU health monitoring through NVML
- XID critical error monitoring
- `ListAndWatch` health updates
- kubelet re-registration
- plugin socket recovery
- duplicate GPU protection
- allocation validation
- graceful shutdown
- Kubernetes DaemonSet deployment
- automated container builds through GitHub Actions
- container images published to GHCR

## Example

A node containing:

```text
GPU 0: NVIDIA GeForce RTX 3060 Ti
GPU 1: NVIDIA GeForce GTX 1660 SUPER
```

is exposed to Kubernetes as:

```text
gpu.local/rtx-3060-ti:    1
gpu.local/gtx-1660-super: 1
```

A workload can request the RTX 3060 Ti directly:

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

The allocation path is:

```text
Pod
 |
 | gpu.local/rtx-3060-ti: 1
 |
 v
Kubernetes Scheduler
 |
 v
kubelet
 |
 v
k8s-gpu-device-plugin
 |
 v
Physical GPU UUID
 |
 v
NVIDIA CDI
 |
 v
Container
```

## Deployment

The plugin is designed to run as a Kubernetes DaemonSet on GPU nodes.

```bash
kubectl apply -f deploy/daemonset.yaml
```

Check the plugin Pods:

```bash
kubectl get pods -A -l app=k8s-gpu-device-plugin -o wide
```

Check the GPU resources advertised by a node:

```bash
kubectl get node <node-name> -o json | jq '.status.capacity, .status.allocatable'
```

Example:

```text
gpu.local/gtx-1660-super: 1
gpu.local/rtx-3060-ti:    1
```

Remove the DaemonSet:

```bash
kubectl delete -f deploy/daemonset.yaml
```

## Container Image

Container images are built automatically by GitHub Actions and published to GHCR:

```text
ghcr.io/maxagateff/k8s-gpu-device-plugin
```

Development image:

```text
ghcr.io/maxagateff/k8s-gpu-device-plugin:latest
```

## Build From Source

The Go module is located in `src/`.

```bash
git clone https://github.com/maxagateff/k8s-gpu-device-plugin.git
cd k8s-gpu-device-plugin
git checkout develop
cd src
```

Verify and build:

```bash
go mod verify
go vet ./...
go test ./...
go build -o k8s-gpu-device-plugin .
```

## Requirements

GPU nodes require:

- Linux
- Kubernetes / kubelet
- NVIDIA GPU
- NVIDIA driver
- NVML
- NVIDIA Container Toolkit
- NVIDIA CDI configuration
- CDI-capable container runtime

Basic host checks:

```bash
nvidia-smi
nvidia-smi -L
nvidia-ctk cdi list
```

## Project Layout

```text
k8s-gpu-device-plugin/
├── .github/
│   └── workflows/
├── deploy/
│   └── daemonset.yaml
├── docs/
├── src/
│   ├── cdi.go
│   ├── gpu.go
│   ├── main.go
│   ├── manager.go
│   ├── plugin.go
│   ├── go.mod
│   └── go.sum
└── Dockerfile
```

## Documentation

Detailed documentation covering architecture, configuration, GPU discovery, resource naming, CDI integration, allocation, health monitoring, kubelet lifecycle handling, troubleshooting, limitations, and deployment considerations is available in:

```text
docs/docs.md
```

## Status

The current implementation has been validated end-to-end with NVIDIA GeForce RTX 3060 Ti and GTX 1660 SUPER GPUs.

The validated path includes:

```text
NVML discovery
      |
      v
kubelet registration
      |
      v
Kubernetes extended resource
      |
      v
Pod GPU request
      |
      v
Device Plugin Allocate()
      |
      v
CDI allocation
      |
      v
Correct physical GPU inside the container
```

## License

See `docs/LICENSE`.