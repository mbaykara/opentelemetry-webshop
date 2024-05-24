# Use an official Golang runtime as a parent image
FROM golang:1.22-alpine AS build

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Build the Go app
RUN go build -o app .

FROM alpine:3.19

# Add a non-root user and group
RUN addgroup -S appgroup && adduser -S appuser -G appgroup
WORKDIR /app
RUN chown -R appuser:appgroup /app
USER appuser

COPY --from=build /app/app .
EXPOSE 8090
CMD ["./app"]
