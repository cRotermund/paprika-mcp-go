# Local Development With Kubernetes

This guide walks through building images and running them on a local
Kubernetes cluster. For running services directly with `go run`, see
[CONTRIBUTING.md](../CONTRIBUTING.md).

All commands run from the repository root.

## 1. Start a local cluster

Pick one:

| Cluster             | Notes                                                             |
| ------------------- | ----------------------------------------------------------------- |
| **Rancher Desktop** | Ships with Kubernetes built in. Easiest on Windows and macOS.     |
| **minikube**        | Most configurable. Needs images loaded with `minikube image load`. |
| **kind**            | Runs Kubernetes in Docker. Needs images loaded with `kind load`.  |

Confirm `kubectl` points at the local cluster:

```sh
kubectl config current-context
```

## 2. Build images

Images are tagged `localhost/<service>:latest`, which is what the `local`
overlay expects:

```sh
docker build -f src/services/paprika-mcp-go/Dockerfile -t localhost/paprika-mcp-go:latest .
```

Load the image into clusters that do not share your container runtime:

```sh
minikube image load localhost/paprika-mcp-go:latest
kind load docker-image localhost/paprika-mcp-go:latest
```

## 3. Deploy

```sh
kubectl apply -k infra/k8s/local
kubectl get pods
```

## 4. Reach the service

```sh
kubectl port-forward deployment/paprika-mcp-go 8080:8080
curl http://localhost:8080/health
```

## 5. Tear down

```sh
kubectl delete -k infra/k8s/local
```

## Container runtime notes

| Issue                                           | Solution                                                         |
| ----------------------------------------------- | ---------------------------------------------------------------- |
| Docker Desktop licensing                        | Use Podman or Rancher Desktop.                                   |
| Podman builds fail with SELinux errors          | `podman build --security-opt label=disable ...`                  |
| Podman cannot load images into kind             | `podman save <img> \| kind load image-archive /dev/stdin`        |
| Docker Hub rate limits                          | Log in to Docker Hub or configure a pull-through mirror.         |
| Rancher Desktop networking broken on a VPN      | Use minikube with `--driver=docker`.                             |
