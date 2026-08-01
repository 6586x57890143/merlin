# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/merlin ./cmd/bot

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/merlin /merlin
USER nonroot:nonroot
ENTRYPOINT ["/merlin"]
