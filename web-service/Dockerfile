# syntax=docker/dockerfile:1

# ---- front end -------------------------------------------------------------
# The React app is compiled here and only its static output travels on, so the
# final image holds no Node, no node_modules and no source maps of third parties.
FROM node:22-alpine AS ui
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY frontend/ ./
# vite.config.ts writes to ../internal/ui/dist
RUN npm run build

# ---- gateway ---------------------------------------------------------------
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
COPY --from=ui /src/internal/ui/dist ./internal/ui/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/web-service .

# ---- run -------------------------------------------------------------------
# scratch: the binary (with the app embedded in it) is the whole image. It keeps
# no data, so unlike the other services there is no volume.
FROM scratch
COPY --from=build /out/web-service /web-service
USER 65532:65532
ENV PORT=3000
EXPOSE 3000
HEALTHCHECK --interval=10s --timeout=3s --start-period=3s --retries=3 \
  CMD ["/web-service", "-healthcheck"]
ENTRYPOINT ["/web-service"]
