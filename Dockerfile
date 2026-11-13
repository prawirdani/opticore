# Build stage.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/opticore ./cmd/opticore

# Runtime: static, non-root, CA certificates for https hooks.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/opticore /opticore
USER nonroot:nonroot
EXPOSE 7547 8081
ENTRYPOINT ["/opticore"]
CMD ["serve"]
