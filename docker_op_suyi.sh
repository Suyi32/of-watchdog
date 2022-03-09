make
docker build --build-arg TARGETARCH=amd64 -t smarterlsy/of-watchdog .
docker push smarterlsy/of-watchdog
