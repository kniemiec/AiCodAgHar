FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -ldflags "-X github.com/kniemiec/AiCodAgHar/internal/buildinfo.Version=${VERSION} -X github.com/kniemiec/AiCodAgHar/internal/buildinfo.Commit=${COMMIT}" -o /out/control-plane ./cmd/control-plane

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/control-plane /control-plane
ENTRYPOINT ["/control-plane"]
