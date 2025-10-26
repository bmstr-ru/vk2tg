FROM golang:1.25.3 AS builder
WORKDIR /src

COPY go.mod ./
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /out/vk2tg ./cmd/vk2tg

FROM gcr.io/distroless/base-debian12
WORKDIR /app

COPY --from=builder /out/vk2tg ./vk2tg
COPY index.html ./index.html

EXPOSE 8080

USER nonroot

ENTRYPOINT ["/app/vk2tg"]
