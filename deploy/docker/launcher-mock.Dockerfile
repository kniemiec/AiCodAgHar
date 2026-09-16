FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/launcher-mock ./mocks/launcher-mock

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/launcher-mock /launcher-mock
ENTRYPOINT ["/launcher-mock"]
