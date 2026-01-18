# UPDATED: Using Go 1.25 to match the new library
FROM golang:1.25-alpine

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./

RUN go build -o /server

CMD ["/server"]
