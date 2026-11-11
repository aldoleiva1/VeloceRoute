FROM alpine:3.20
RUN apk add --no-cache iproute2 iputils tcpdump
COPY gobgpd gobgp /usr/local/bin/
COPY feedgen-linux /usr/local/bin/feedgen
ENTRYPOINT []
CMD ["sleep", "infinity"]
