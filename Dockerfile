FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
COPY static ./static
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /winboat-web .

FROM alpine:3.22
COPY --from=build /winboat-web /winboat-web
USER 65532:65532
EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=3s \
  CMD wget -qO- http://127.0.0.1:8000/health || exit 1
ENTRYPOINT ["/winboat-web"]
