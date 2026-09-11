FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/control-plane ./cmd/control-plane

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/control-plane /control-plane
ENTRYPOINT ["/control-plane"]
