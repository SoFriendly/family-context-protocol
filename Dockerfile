# Build: a static binary (pure-Go SQLite, no CGO).
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /fcp ./cmd/fcp && mkdir /data

# Run: nothing but the binary.
FROM scratch
COPY --from=build /fcp /fcp
COPY --from=build --chown=65532:65532 /data /data
USER 65532:65532
ENV FCP_LISTEN=0.0.0.0:8080 FCP_KEYS=/data/fcp-keys.json
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD ["/fcp", "health"]
ENTRYPOINT ["/fcp"]
CMD ["serve"]
