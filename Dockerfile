FROM golang:1.25.5-alpine AS builder

WORKDIR /app

# Install git if needed for modules
RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build for amd64 specifically
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o unas-fan-controller main.go

FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/unas-fan-controller .

EXPOSE 8080
ENTRYPOINT ["/app/unas-fan-controller"]
