# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/merlin ./cmd/bot

# Pinned by digest, not just by tag.
#
# CI failed once on "403 Forbidden" from a HEAD against this manifest, minutes
# after the identical Dockerfile had built fine on the branch. The image had
# not moved and is anonymously pullable; gcr.io throttles anonymous requests
# from the shared egress IPs that GitHub's runners come from, and answers 403
# rather than 429 when it does. A digest cannot prevent that, but it removes
# the tag lookup from the path, and it is what makes a rebuild of an old
# commit produce the same image rather than whatever :nonroot points at that
# week. Re-running the job is the remedy when it does happen.
#
# gcr.io/distroless/static-debian12:nonroot as of 2026-08-26. To update:
#   crane digest gcr.io/distroless/static-debian12:nonroot
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=builder /out/merlin /merlin
USER nonroot:nonroot
ENTRYPOINT ["/merlin"]
