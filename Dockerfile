# syntax=docker/dockerfile:1

FROM golang:1.27 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/gateway ./cmd/gateway
COPY gateway ./gateway

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=build /out/gateway /gateway

USER 65532:65532
EXPOSE 8080 9091

ENTRYPOINT ["/gateway"]
CMD ["-addr=:8080", "-registry=/etc/vire/models.json", "-metrics-addr=:9091"]
