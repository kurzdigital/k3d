# Running CUDA workloads

If you want to run CUDA workloads on the K3s container you need to customize the container.  
CUDA workloads require the NVIDIA Container Runtime, so containerd needs to be configured to use this runtime.  
The K3s container itself also needs to run with this runtime.  
If you are using Docker you can install the [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html).

## Choosing how to expose the GPU

k3d offers five flags around GPU and device passthrough. Pick the right one(s) based on what your host runtime supports:

| Flag | When to use | Example |
|------|-------------|---------|
| `--gpus` | Legacy NVIDIA Container Toolkit hook (works with the classic `nvidia` Docker runtime). Wires `DeviceRequest` with the NVIDIA driver. | `--gpus all` |
| `--device <CDI ID>` | Modern, Docker-native. Requires Docker 25+ and `nvidia-container-toolkit` configured in CDI mode (`/etc/cdi/*.yaml`). | `--device nvidia.com/gpu=all` |
| `--device <path>` | Anything else: AMD ROCm (`/dev/kfd`, `/dev/dri`), Intel iGPU, FPGAs, FUSE, `/dev/kvm`, ... Plain host device passthrough. | `--device /dev/kfd --device /dev/dri:/dev/dri:rwm` |
| `--runtime` | Use a non-default Docker runtime for the node containers. Required for the classic `nvidia` runtime if you need it to inject Vulkan ICD files (`/etc/vulkan/icd.d`) and libraries — neither `--gpus` nor `--device` does that. Also useful for `crun`, `kata`, etc. | `--runtime nvidia` |
| `--auto-gpu` | Detect the host GPU vendor and apply matching `--gpus`/`--device` automatically. Useful in portable scripts that should work across NVIDIA/AMD/Intel hosts. Skipped if `--gpus` or `--device` is also set. | `--auto-gpu auto` |

The first four flags are additive: `--gpus`, `--device`, and `--runtime` can be combined. A typical Vulkan-on-NVIDIA setup is `--runtime nvidia --gpus all`. `--auto-gpu` is an opt-in convenience layer over the same fields. The rest of this page focuses on the legacy `--gpus` path because that is what the custom CUDA image below depends on.

### `--auto-gpu` detection details

| Environment | Detection chain |
|-------------|-----------------|
| Native Linux | `nvidia-smi` → `/dev/nvidia*` → `lspci -d 10de:/1002:/8086:` → `/sys/class/drm/renderD*/device/vendor` |
| WSL2 | `nvidia-smi` → `vulkaninfo --summary` → `glxinfo -B` → `/dev/dxg` (warns if present but no detection tool installed) |
| macOS | always reports "no GPU" (Docker Desktop's Linux VM has no GPU passthrough) |

Mappings (only applied when no explicit `--gpus`/`--device` is set):

- **NVIDIA** → `--gpus all` (legacy NVIDIA Container Toolkit hook). For Vulkan/ICD, also pass `--runtime nvidia`. For Docker 25+ with CDI, override the auto behavior with `--device nvidia.com/gpu=all`.
- **AMD** → `--device /dev/dri:/dev/dri:rwm`, plus `--device /dev/kfd` if that node exists on the host (ROCm compute).
- **Intel** → `--device /dev/dri:/dev/dri:rwm`.

Accepted values for `--auto-gpu`: `auto` (run detection), `nvidia` / `amd` / `intel` (skip detection, apply mapping directly — useful when detection tools aren't installed), `none` (default, opt-out).

!!! warning "Remote Docker daemons"
    `--auto-gpu auto` runs its detection probes on the **client** machine, while the resulting device mappings are applied on the **daemon** host. If your Docker daemon is remote (e.g. via `DOCKER_HOST` or a remote context), detection will inspect the wrong machine and produce wrong results — use explicit `--gpus` / `--device` flags instead.

For NVIDIA hosts, the mapping is only applied if the daemon actually supports NVIDIA passthrough (a registered CDI spec or an `nvidia` runtime). If a GPU is detected but the NVIDIA Container Toolkit is not set up on the daemon, k3d warns and skips the passthrough instead of creating a cluster that would fail to start.

## Building a customized K3s image

To get the NVIDIA container runtime in the K3s image you need to build your own K3s image.  
The native K3s image is based on Alpine but the NVIDIA container runtime is not supported on Alpine yet.  
To get around this we need to build the image with a supported base image.

### Dockerfile

[Dockerfile](cuda/Dockerfile):  

```Dockerfile
{%
  include-markdown "./cuda/Dockerfile"
  comments=false
%}
```

This Dockerfile is based on the [K3s Dockerfile](https://github.com/rancher/k3s/blob/master/package/Dockerfile)
The following changes are applied:

1. Change the base images to nvidia/cuda:12.4.1-base-ubuntu22.04 so the NVIDIA Container Toolkit can be installed. The version of `cuda:xx.x.x` must match the one you're planning to use.
2. Add a manifest for the NVIDIA driver plugin for Kubernetes with an added RuntimeClass definition. See [k3s documentation](https://docs.k3s.io/advanced#nvidia-container-runtime-support).

### The NVIDIA device plugin

To enable NVIDIA GPU support on Kubernetes you also need to install the [NVIDIA device plugin](https://github.com/NVIDIA/k8s-device-plugin). The device plugin is a daemonset and allows you to automatically:

* Expose the number of GPUs on each nodes of your cluster
* Keep track of the health of your GPUs
* Run GPU enabled containers in your Kubernetes cluster.

```yaml
{%
  include-markdown "./cuda/device-plugin-daemonset.yaml"
  comments=false
%}
```

Two modifications have been made to the original NVIDIA daemonset:

1. Added RuntimeClass definition to the YAML frontmatter.

   ```yaml
   apiVersion: node.k8s.io/v1
   kind: RuntimeClass
   metadata:
     name: nvidia
   handler: nvidia
   ```

2. Added `runtimeClassName: nvidia` to the Pod spec.

Note: you must explicitly add `runtimeClassName: nvidia` to all your Pod specs to use the GPU. See [k3s documentation](https://docs.k3s.io/advanced#nvidia-container-runtime-support).

### Build the K3s image

To build the custom image we need to build K3s because we need the generated output.

Put the following files in a directory:

* [Dockerfile](cuda/Dockerfile)
* [device-plugin-daemonset.yaml](cuda/device-plugin-daemonset.yaml)
* [build.sh](cuda/build.sh)
* [cuda-vector-add.yaml](cuda/cuda-vector-add.yaml)

The `build.sh` script is configured using exports & defaults to `v1.28.8+k3s1`. Please set at least the `IMAGE_REGISTRY` variable! The script performs the following steps builds the custom K3s image including the nvidia drivers.

[build.sh](cuda/build.sh):

```bash
{%
  include-markdown "./cuda/build.sh"
  comments=false
%}
```

## Run and test the custom image with k3d

You can use the image with k3d:

```bash
k3d cluster create gputest --image=$IMAGE --gpus=1
```

Deploy a [test pod](cuda/cuda-vector-add.yaml):

```bash
kubectl apply -f cuda-vector-add.yaml
kubectl logs cuda-vector-add
```

This should output something like the following:

```bash
$ kubectl logs cuda-vector-add

[Vector addition of 50000 elements]
Copy input data from the host memory to the CUDA device
CUDA kernel launch with 196 blocks of 256 threads
Copy output data from the CUDA device to the host memory
Test PASSED
Done
```

If the `cuda-vector-add` pod is stuck in `Pending` state, probably the device-driver daemonset didn't get deployed correctly from the auto-deploy manifests. In that case, you can apply it manually via `#!bash kubectl apply -f device-plugin-daemonset.yaml`.

## Acknowledgements

Most of the information in this article was obtained from various sources:

* [Add NVIDIA GPU support to k3s with containerd](https://dev.to/mweibel/add-nvidia-gpu-support-to-k3s-with-containerd-4j17)
* [microk8s](https://github.com/ubuntu/microk8s)
* [K3s](https://github.com/rancher/k3s)
* [k3s-gpu](https://gitlab.com/vainkop1/k3s-gpu)

## Authors

* [@markrexwinkel](https://github.com/markrexwinkel)
* [@vainkop](https://github.com/vainkop)
* [@iwilltry42](https://github.com/iwilltry42)
* [@dbreyfogle](https://github.com/dbreyfogle)
