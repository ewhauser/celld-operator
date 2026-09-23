# Local integration image. celld is built by hack/build-preview-runtime.sh.
FROM rust:1.97.1-bookworm AS certificates
FROM debian:bookworm-slim
COPY --from=certificates /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY celld /usr/local/bin/celld
ENTRYPOINT ["/usr/local/bin/celld"]
