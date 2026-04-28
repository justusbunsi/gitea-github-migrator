FROM golang:1.26.2-alpine@sha256:f85330846cde1e57ca9ec309382da3b8e6ae3ab943d2739500e08c86393a21b1 AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY ./ ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-w -s" -o /gitea-github-migrator \
    && adduser -DHu 10001 gitea-github-migrator \
    && grep gitea-github-migrator /etc/passwd > /etc/passwd_minified \
    && mkdir /persistence

FROM scratch
COPY --from=build /etc/passwd_minified /etc/passwd
COPY --from=build /etc/ssl /etc/ssl
USER gitea-github-migrator
COPY --chmod=0755 --from=build /persistence /persistence
COPY --chmod=0511 --from=build /gitea-github-migrator /gitea-github-migrator
ENTRYPOINT ["/gitea-github-migrator"]
VOLUME ["/persistence"]
