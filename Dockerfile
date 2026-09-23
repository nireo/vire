# syntax=docker/dockerfile:1

FROM node:26-alpine AS web-build

WORKDIR /src/web
RUN npm install --global pnpm@10.33.0
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

FROM golang:1.27 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/gateway ./cmd/gateway
COPY cmd/vire-admin ./cmd/vire-admin
COPY gateway ./gateway
COPY internal ./internal

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/vire-admin ./cmd/vire-admin

FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=build /out/gateway /gateway
COPY --from=build /out/vire-admin /vire-admin
COPY --from=web-build /src/web/dist /web

USER 65532:65532
EXPOSE 8080 9091

ENTRYPOINT ["/gateway"]
CMD ["-addr=:8080", "-registry=/etc/vire/models.json", "-metrics-addr=:9091", "-web-dir=/web"]
