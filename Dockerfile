# syntax=docker/dockerfile:1
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ ./cmd/...

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
COPY pricing/prices.yaml /etc/switchyard/prices.yaml
COPY configs/demo.yaml /etc/switchyard/switchyard.yaml
ENV SWITCHYARD_CONFIG=/etc/switchyard/switchyard.yaml
EXPOSE 8080 9091
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/switchyard"]
