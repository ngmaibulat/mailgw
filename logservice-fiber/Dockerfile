# BUILD CONTEXT IS THE REPO ROOT, not this directory.
#
# go.mod carries `replace ... => ../logservice-go`, and Go reads a replacement's
# go.mod during module resolution — so the sibling module has to be inside the
# context or the build fails with "replacement directory ... does not exist".
# That is a difference from logservice-go's own Dockerfile, which builds from
# its own directory; the precedent for a root context is webui-fastify's, which
# needs the workspace lockfile for the same structural reason.
#
#     docker buildx build -f logservice-fiber/Dockerfile .
#
# container-build.sh does this for you.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Both manifests first, and the replacement's too: `go mod download` resolves
# the module graph, which means reading ../logservice-go/go.mod. Copying only
# this module's pair would fail here rather than at the build step.
COPY logservice-go/go.mod logservice-go/go.sum ./logservice-go/
COPY logservice-fiber/go.mod logservice-fiber/go.sum ./logservice-fiber/
RUN cd logservice-fiber && go mod download

COPY logservice-go ./logservice-go
COPY logservice-fiber ./logservice-fiber

ARG VERSION=dev
ARG COMMIT=none
RUN cd logservice-fiber && CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/logservice-fiber ./cmd/logservice-fiber

# Same runtime image as logservice-go: fasthttp is pure Go, so CGO_ENABLED=0 and
# distroless/static still hold. No shell here either — a compose HEALTHCHECK
# cannot be a curl.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/logservice-fiber /usr/local/bin/logservice-fiber

USER nonroot:nonroot

EXPOSE 3000

ENTRYPOINT ["/usr/local/bin/logservice-fiber"]

# No CMD: the default subcommand is `serve`. Configured entirely by its
# environment — PORT, API_KEY, DB_* — exactly as logservice-go is, because it is
# the same service with a different HTTP layer and an operator must be able to
# swap one for the other without editing anything but an image tag.
