# Build every command in cmd/ into a static binary, then ship them in an
# image with no shell, no package manager and no compiler.
#
# Dependencies are vendored into the repository, so the build resolves
# nothing over the network and needs no separate download layer.
FROM golang:1.23-alpine AS build

WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -mod=vendor -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/ /usr/local/bin/

# An explicit working directory, so the data root check has a repository
# root to compare against that is not "/".
WORKDIR /app

# The data root is mounted at run time, never copied in: it holds personal
# data that must not enter an image or its build context.
ENV ROBASKO_DATA_DIR=/data
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/agentd"]
