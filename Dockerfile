# Build stage
FROM golang:1.25-alpine3.24 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 go build -ldflags="-w -s" -o llm-api-proxy main.go

# Run stage
FROM alpine:3.24
RUN apk --no-cache add ca-certificates
# Create a non-root user and setup a directory for files/database
RUN addgroup -S appgroup && adduser -S appuser -G appgroup
WORKDIR /app
COPY --from=builder /app/llm-api-proxy .
RUN chown -R appuser:appgroup /app
USER appuser
EXPOSE 8318
ENTRYPOINT ["./llm-api-proxy"]
