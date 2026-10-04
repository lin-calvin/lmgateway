FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=build /out/gateway /app/gateway
COPY config /app/config

EXPOSE 8080
ENTRYPOINT ["/app/gateway"]
