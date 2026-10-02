# syntax=docker/dockerfile:1
# Build from the repository root:
#   docker build -f services/inventory/Dockerfile -t inventory-service .

FROM golang:1.26-alpine AS build
WORKDIR /src

# Download dependencies first so this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w \
      -X github.com/HuzaifaMH/go-ecommerce-microservices/pkg/version.Version=${VERSION} \
      -X github.com/HuzaifaMH/go-ecommerce-microservices/pkg/version.Commit=${COMMIT}" \
    -o /out/inventory ./services/inventory/cmd/inventory

# Static binary on a minimal, non-root base image.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/inventory /inventory
USER nonroot:nonroot
EXPOSE 9090 8080
ENTRYPOINT ["/inventory"]
