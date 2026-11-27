# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:5f1b98fa02fe396ebce2a0227a0e08309e11792915b9dd2e1f71053d7e7e6666 AS build
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE" -o /out/chatkeeper ./cmd/chatkeeper
RUN mkdir -p /out/data && chown 65532:65532 /out/data
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/chatkeeper /chatkeeper
COPY --from=build --chown=65532:65532 /out/data /data
ENV CHATKEEPER_HOME=/data CHATKEEPER_MODE=server
USER 65532:65532
EXPOSE 8787
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/chatkeeper", "healthcheck"]
ENTRYPOINT ["/chatkeeper"]
CMD ["run", "--mode", "server", "--bind", "0.0.0.0:8787", "--no-browser"]
