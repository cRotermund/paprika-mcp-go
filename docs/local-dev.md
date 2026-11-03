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

The local overlay generates a `paprika-mcp-go` Secret from
`infra/k8s/local/paprika-mcp-go.env.example`. It holds `MCP_TOKEN_SHA256`, the
hash of the throwaway token `local-dev-token`, so call `/mcp` with
`Authorization: Bearer local-dev-token`. Use a real generated token anywhere else.
The same file holds placeholder `PAPRIKA_EMAIL` and `PAPRIKA_PASSWORD` values: the pod starts and
answers `/health`, but sign-in to Paprika fails until you replace them. Use the Paprika test account
in a local edit that you do not commit.

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

## Index store integration tests

The S3 protocol tests in `src/services/paprika-mcp-go/internal/indexstore` run the real
AWS SDK against [SeaweedFS](https://github.com/seaweedfs/seaweedfs) with authentication
turned on. They sit behind the `integration` build tag and fail, rather than skip, when
the emulator is unreachable. Start the same pinned image CI uses, from the repository root:

```sh
docker run -d --name seaweedfs -p 8333:8333 \
  -v "$PWD/src/services/paprika-mcp-go/internal/indexstore/testdata/seaweedfs-s3.json:/etc/seaweedfs/s3.json:ro" \
  chrislusf/seaweedfs:4.51 server -s3 -s3.config=/etc/seaweedfs/s3.json -dir=/data
```

Wait until `curl -s -o /dev/null -w '%{http_code}' http://localhost:8333/` prints `403`
(the S3 port answers, and rejects unsigned requests). Then run:

```sh
cd src/services/paprika-mcp-go
go test -tags integration ./internal/indexstore/...
```

Git Bash on Windows rewrites paths in `-v`; prefix the command with `MSYS_NO_PATHCONV=1`
and use `$(pwd -W)` in place of `$PWD`.

The tests use the identity in `testdata/seaweedfs-s3.json` by default. To target another
emulator, set `INDEXSTORE_S3_ENDPOINT`, `INDEXSTORE_S3_ACCESS_KEY`, and
`INDEXSTORE_S3_SECRET_KEY`. Remove the container with `docker rm -f seaweedfs`.
