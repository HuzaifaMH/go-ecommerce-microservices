# syntax=docker/dockerfile:1
# One Dockerfile for every service; pick it with --build-arg SERVICE=<name>.
# Build from the repository root:
#   docker build -f deploy/docker/Dockerfile --build-arg SERVICE=inventory -t inventory-service .

FROM golang:1.26-alpine AS build
WORKDIR /src

# Download dependencies first so this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG SERVICE
ARG VERSION=dev
ARG COMMIT=unknown
RUN test -n "${SERVICE}" || (echo "build arg SERVICE is required" >&2; exit 1)
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w \
      -X github.com/HuzaifaMH/go-ecommerce-microservices/pkg/version.Version=${VERSION} \
      -X github.com/HuzaifaMH/go-ecommerce-microservices/pkg/version.Commit=${COMMIT}" \
    -o /out/service ./services/${SERVICE}/cmd/${SERVICE}

# Static binary on a minimal, non-root base image.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/service /service
USER nonroot:nonroot
EXPOSE 9090 8080
ENTRYPOINT ["/service"]
