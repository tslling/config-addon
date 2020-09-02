FROM golang:1.13 AS build_base
WORKDIR /go/src/github.com/tslling/config-addon
# copy go module files and download
# cache: only download when go.mod or go.sum changes
COPY go.mod .
COPY go.sum .
RUN go mod download

FROM build_base AS server_builder
# copy rest of files
COPY . .
RUN go build -o config-addon *.go
CMD ["./config-addon"]