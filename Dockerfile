# syntax=docker/dockerfile:1

# Pin the builder to the native build platform and cross-compile via GOARCH;
# otherwise buildx runs the toolchain under QEMU for the arm64 leg (glacial).
FROM --platform=$BUILDPLATFORM docker.io/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
ARG TARGETARCH
RUN CGO_ENABLED=0 GOARCH=${TARGETARCH} go build -trimpath -o /out/manager ./cmd

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
