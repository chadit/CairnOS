# CairnOS container image: multi-stage, static binary, distroless runtime,
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
# distroless static. The image ships third-party code, and MIT, BSD, and
# Apache-2.0 require their license and NOTICE texts to travel with it, so they
# are generated from the versions go.mod pins for the platform this stage builds.
RUN CGO_ENABLED=0 go build -o /out/cairnos ./cmd/cairnos \
    && sh scripts/third-party-notices.sh > /out/THIRD_PARTY_NOTICES.txt

# digest-pin: skip dev image; digests pinned at first release
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/cairnos /usr/local/bin/cairnos
COPY --from=builder /src/LICENSE /out/THIRD_PARTY_NOTICES.txt /usr/share/doc/cairnos/

USER 65532:65532

# 7777 serves gRPC, 8080 the HTTP API, and 9090 Prometheus metrics.
EXPOSE 7777 8080 9090

VOLUME ["/data"]

HEALTHCHECK CMD ["/usr/local/bin/cairnos", "health", "--dir", "/data"]

ENTRYPOINT ["/usr/local/bin/cairnos"]

# The API server is the long-lived process; the scheduler daemon runs inside
# serve.
CMD ["serve", "--dir", "/data"]
