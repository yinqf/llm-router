FROM golang:1.25.4-alpine AS build

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /bin/llm-router ./

FROM alpine:3.20

WORKDIR /app

COPY --from=build /bin/llm-router /usr/local/bin/llm-router

RUN apk add --no-cache tzdata \
    && ln -sf /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone

ENV TZ=Asia/Shanghai
EXPOSE 8080
ENV PORT=8080
ENV OPENAI_BASE_URL=https://api.openai.com

CMD ["llm-router"]
