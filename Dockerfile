# CairnOS container image (.agents/spec/11-deployment-and-config.md,
# "Container image"): multi-stage, static binary, distroless runtime,
# non-root, exec-form entrypoint and health check.

# hadolint ignore=DL3007
# The builder tracks the latest Go on Alpine on purpose: each rebuild picks up
# Go security patches and toolchain updates without a manual version bump. That
# trades build reproducibility for currency, delegated to the orchestrator's
# redeploy-on-image-change flow; the builder stage is discarded, so only the
# runtime image below ships.
# digest-pin: skip builder tracks latest Go, so there is no fixed digest to pin
FROM golang:alpine AS builder

WORKDIR /src

# Dependency download sits in its own layer so source edits do not refetch
# modules.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 keeps the binary static so the runtime stage stays on
# distroless static.
RUN CGO_ENABLED=0 go build -o /out/cairnos ./cmd/cairnos

# digest-pin: skip Phase 0 dev image; digests pinned at first release
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/cairnos /usr/local/bin/cairnos

USER 65532:65532

# gRPC, HTTP API, and Prometheus metrics per the spec's port assignments.
EXPOSE 7777 8080 9090

VOLUME ["/data"]

HEALTHCHECK CMD ["/usr/local/bin/cairnos", "health", "--dir", "/data"]

ENTRYPOINT ["/usr/local/bin/cairnos"]

# The API server is the long-lived process; the scheduler daemon runs inside
# serve.
CMD ["serve", "--dir", "/data"]
