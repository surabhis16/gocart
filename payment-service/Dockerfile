# syntax=docker/dockerfile:1

# ---- build -----------------------------------------------------------------
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/payment-service . \
 && mkdir /out/data

# ---- run -------------------------------------------------------------------
# scratch: no shell, no package manager, nothing to patch. The binary is the
# whole image, which is why it carries its own -healthcheck flag.
FROM scratch
COPY --from=build /out/payment-service /payment-service
# A volume mounted here inherits this ownership, so the non-root user can write.
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV PORT=8082 DATA_DIR=/data
EXPOSE 8082
HEALTHCHECK --interval=10s --timeout=3s --start-period=3s --retries=3 \
  CMD ["/payment-service", "-healthcheck"]
ENTRYPOINT ["/payment-service"]
