# Build context is mailgw-go/ itself — a Go module needs no workspace lockfile,
# so this follows the logservice pattern rather than mailgw's root-context build.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Cached separately so a source-only change does not re-download modules.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/mailgw-go ./cmd/mailgw-go

# distroless/static ships CA certificates, which the outbound TLS client needs.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/mailgw-go /usr/local/bin/mailgw-go

USER nonroot:nonroot

# 2525 rather than 25: the nonroot user cannot bind a privileged port. Map it in
# compose if port 25 is wanted on the host.
EXPOSE 2525

ENTRYPOINT ["/usr/local/bin/mailgw-go"]
CMD ["-config", "/opt/mailgw-go/config"]
