FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

# The build context is the repository root so sibling workspace modules can be
# copied in when this service depends on them (see the skill's multi-module notes).
WORKDIR /src/services/paprika-mcp-go
COPY src/services/paprika-mcp-go/go.mod src/services/paprika-mcp-go/go.sum ./
RUN go mod download
COPY src/services/paprika-mcp-go/ ./
RUN CGO_ENABLED=0 GOWORK=off GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /server .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /server /server

EXPOSE 8080
USER 65532:65532

ENTRYPOINT ["/server"]
