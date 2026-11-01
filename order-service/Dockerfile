# syntax=docker/dockerfile:1

# ---- build -----------------------------------------------------------------
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
ARG VERSION=dev
# Empty by default: the default image has no database driver in it and stores
# events in a file. Build with `--build-arg TAGS=cassandra` to compile in the
# Cassandra event store; that fetches the driver, which is why it is opt-in.
ARG TAGS=""
ARG CASSANDRA_DRIVER=github.com/apache/cassandra-gocql-driver/v2@v2.0.0
RUN if echo "${TAGS}" | grep -q cassandra; then go get "${CASSANDRA_DRIVER}" && go mod tidy; fi
RUN CGO_ENABLED=0 go build -trimpath -tags "${TAGS}" \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/order-service . \
 && mkdir /out/data

# ---- run -------------------------------------------------------------------
# scratch: no shell, no package manager, nothing to patch. The binary is the
# whole image, which is why it carries its own -healthcheck flag.
FROM scratch
COPY --from=build /out/order-service /order-service
# A volume mounted here inherits this ownership, so the non-root user can write.
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV PORT=8081 DATA_DIR=/data
EXPOSE 8081
HEALTHCHECK --interval=10s --timeout=3s --start-period=3s --retries=3 \
  CMD ["/order-service", "-healthcheck"]
ENTRYPOINT ["/order-service"]
