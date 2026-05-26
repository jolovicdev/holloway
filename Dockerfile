FROM golang:1.25-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/holloway-server ./cmd/holloway-server

FROM gcr.io/distroless/static-debian12

WORKDIR /app
COPY --from=build /out/holloway-server /app/holloway-server
COPY templates /app/templates
COPY static /app/static

ENV HOLLOWAY_ADDR=:8080
ENV HOLLOWAY_DB=/data/holloway.db
ENV HOLLOWAY_TEMPLATES=/app/templates
ENV HOLLOWAY_STATIC=/app/static

VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["/app/holloway-server"]

