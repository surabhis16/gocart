# syntax=docker/dockerfile:1

# ---- build -----------------------------------------------------------------
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/cart-service . \
 && mkdir /out/data

# ---- run -------------------------------------------------------------------
# scratch: no shell, no package manager, nothing to patch. The binary is the
# whole image, which is why it carries its own -healthcheck flag.
FROM scratch
COPY --from=build /out/cart-service /cart-service
# A volume mounted here inherits this ownership, so the non-root user can write.
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV PORT=8083 DATA_DIR=/data
EXPOSE 8083
HEALTHCHECK --interval=10s --timeout=3s --start-period=3s --retries=3 \
  CMD ["/cart-service", "-healthcheck"]
ENTRYPOINT ["/cart-service"]
