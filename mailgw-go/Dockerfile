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

# The ENGINEERING binary, built only by the `test` stage below. It is a separate
# main package, not a build flag: the shipped binary above does not link
# internal/testctl and CI asserts it with `go list -deps`.
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}-testctl -X main.commit=${COMMIT}" \
    -o /out/mailgw-go-test ./cmd/mailgw-go-test

# Mount points for the queue and data volumes, staged here so the runtime stage
# can COPY them with the right owner. Docker only copies ownership onto a fresh
# named volume when the path already exists in the image, and distroless/static
# contains nothing but the binary — so without this a fresh volume stays
# root:root 0755 and uid 65532 cannot create the spool ("mkdir
# /opt/mailgw-go/queue/tmp: permission denied"). There is no shell in the
# runtime image, so this cannot be fixed with a RUN there.
RUN mkdir -p /seed/queue /seed/data \
    && chmod 0750 /seed/queue \
    && chmod 0700 /seed/data

# ---------------------------------------------------------------------------
# The ENGINEERING image. NEVER deployed, and never built by `pnpm docker:push`.
#
# It carries an unauthenticated HTTP control API for configuring a gateway from
# a test — see internal/testctl/doc.go for why that is acceptable here and
# nowhere else. Build it with mailgw-go/container-build-test.sh, which tags it
# ngmaibulat/mailgw-go-test and deliberately never tags :latest, so no compose
# file can pull it by accident.
#
# It is a separate STAGE rather than a separate Dockerfile so that the runtime
# layout stays defined in one place: a test node whose spool or data directory
# differed from a real one would be testing the wrong thing.
#
# It sits BEFORE the shipped stage on purpose. A multi-stage build with no
# --target produces the LAST stage, so putting this one last would make a bare
# `docker build .` hand somebody the unauthenticated image — precisely the
# accident the separate binary exists to prevent. Reaching it requires
# --target test, which container-build-test.sh passes and nothing else does.
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS test

COPY --from=build /out/mailgw-go-test /usr/local/bin/mailgw-go-test

COPY --from=build --chown=65532:65532 /seed/queue /opt/mailgw-go/queue
COPY --from=build --chown=65532:65532 /seed/data /var/lib/mailgw-go

USER nonroot:nonroot

# 9090 is the test control API, on top of the SMTP and admin ports above.
EXPOSE 2525 8080 9090

ENTRYPOINT ["/usr/local/bin/mailgw-go-test"]

# Unlike the shipped image this one DOES need arguments: -testctl has no
# default, deliberately, so that this binary cannot be dropped in for the
# shipped one and silently open an unauthenticated configuration API.
CMD ["-testctl", "0.0.0.0:9090"]

# distroless/static ships CA certificates, which the outbound TLS client needs.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/mailgw-go /usr/local/bin/mailgw-go

# Empty, correctly-owned mount points — see the build stage for why.
COPY --from=build --chown=65532:65532 /seed/queue /opt/mailgw-go/queue
COPY --from=build --chown=65532:65532 /seed/data /var/lib/mailgw-go

USER nonroot:nonroot

# 2525 rather than 25: the nonroot user cannot bind a privileged port. Map it in
# compose if port 25 is wanted on the host.
# 8080 is the local admin UI (wizard + status). It is locked by a claim code the
# gateway mints on first boot and writes to its log — see README.md. Restrict it
# to the management network at the firewall anyway; see the note below.
EXPOSE 2525 8080

ENTRYPOINT ["/usr/local/bin/mailgw-go"]

# No CMD, deliberately: this image is a ZERO-CONFIGURATION node.
#
# With no arguments the binary runs in managed mode on its compiled-in defaults
# (-data /var/lib/mailgw-go, -admin 0.0.0.0:8080). It generates an Ed25519
# identity on first boot, serves the provisioning wizard, and takes everything
# else from Central Management into the SQLite store on the data volume. No
# environment variables, no flags, no configuration files on the host.
#
# It does not serve SMTP until a deployed configuration applies. That is the
# point: a gateway with no allowlist would deny every peer anyway, and a
# listener that can only reject looks healthy to a load balancer.
#
# There is no second configuration source. `-config <dir>` used to run this
# binary from a directory of files, kept for CI and the e2e suites; it is gone,
# along with every environment variable the gateway once read. Central
# Management is the only thing that can configure a gateway.
#
# The admin UI on 8080 is always listening, and is locked by a claim code. The
# firewall is still required, not optional: what an authenticated caller can do
# is point this gateway at a Central Manager of their choosing and be handed its
# relay credentials, and on an edge node this is a root process with no port
# mapping to narrow. deploy/gateway/05-firewall.sh does it.
