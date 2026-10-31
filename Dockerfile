FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /backup .

FROM mysql:8.4
COPY --from=build /backup /usr/local/bin/mysql-s3-backup
USER mysql
ENTRYPOINT ["/usr/local/bin/mysql-s3-backup"]
