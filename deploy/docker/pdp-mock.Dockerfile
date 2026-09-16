FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/pdp-mock ./mocks/pdp-mock

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/pdp-mock /pdp-mock
ENTRYPOINT ["/pdp-mock"]
