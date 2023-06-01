FROM golang:1.20 AS builder
#RUN apk add --no-cache ca-certificates git
RUN mkdir -p /tmp
WORKDIR /src
# restore dependencies
COPY . .
COPY go.mod go.sum ./
RUN --mount=type=cache,mode=0755,target=/go/pkg/mod go mod vendor

RUN cd ./cmd/otelcontribcol && go build .

FROM ubuntu:20.04 as release
RUN apt-get update && apt-get install \
    ca-certificates \
    curl \
    gnupg \
    lsb-release -y

#FROM ibmjava:11-jdk as release
#FROM adoptopenjdk/openjdk11:debian-jre as release
#RUN apk add --no-cache ca-certificates git
COPY --from=builder /src/cmd/otelcontribcol/otelcontribcol /otelcol-contrib
#COPY opentelemetry-jmx-metrics.jar /opt/opentelemetry-java-contrib-jmx-metrics.jar
EXPOSE 4317 55680 55679
ENTRYPOINT ["/otelcol-contrib"]
CMD ["--config", "/etc/otel/config.yaml"]
