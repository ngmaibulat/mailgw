# Build context is logservice-go/ itself — a Go module needs no workspace
# lockfile, so this follows mailgw-go's own-directory build rather than the
# webui's repo-root one.
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
    -o /out/logservice ./cmd/logservice

# distroless/static ships CA certificates and nothing else. There is no shell,
# which is why a compose HEALTHCHECK cannot be a curl — see README.md.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/logservice /usr/local/bin/logservice

USER nonroot:nonroot

EXPOSE 3000

ENTRYPOINT ["/usr/local/bin/logservice"]

# No CMD: the default subcommand is `serve`, which applies any pending
# migrations and then binds. The db-migrator service overrides this with
# ["migrate"], which migrates and exits — the console depends on that
# service completing, and the console never talks to logservice at boot.
#
# Unlike mailgw-go, this image IS configured by its environment: PORT, API_KEY
# and the DB_* variables. That is not a lapse from the zero-configuration rule,
# it is the other side of it — a gateway runs on a host its operator does not
# configure, this runs beside the database whose credentials it needs.
