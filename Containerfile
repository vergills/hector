FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /hector ./cmd/hector

FROM alpine:3.22

RUN apk add --no-cache grep
COPY --from=build /hector /usr/local/bin/hector
WORKDIR /workspace
USER nobody
ENTRYPOINT ["/usr/local/bin/hector"]
