# Build stage
FROM golang:1.21-alpine AS builder

WORKDIR /app
COPY . .
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o peakshield .

# Final stage
FROM scratch
WORKDIR /app
COPY --from=builder /app/peakshield .
EXPOSE 8080
ENTRYPOINT ["/app/peakshield"]
