# Stage 1: Build the mariadb-connector-c
FROM alpine as connector-builder
# FROM alpine

# Install build dependencies
RUN apk update
RUN apk add git cmake make gcc g++ openssl-dev

RUN git clone https://github.com/mariadb-corporation/mariadb-connector-c.git

# Compile the connector
WORKDIR /mariadb-connector-c
RUN cmake . -DWITH_SSL=OPENSSL -DCMAKE_C_FLAGS="-Wno-error=cpp" -DCMAKE_INSTALL_PREFIX=/opt \
    && make \
    && make install


# CMD ["/bin/sh"]

# Stage 2: Build your application image
FROM alpine

# Continue with your application setup...
# Set the working directory inside the container
WORKDIR /opt

# Install mysql-client
RUN apk update && apk add mysql-client
RUN apk add openssl

# # Copy the compiled connector from the build stage
COPY --from=connector-builder /mariadb-connector-c/caching_sha2_password.so /usr/lib/mariadb/plugin/

COPY ./containers/mysql-cli/run.sh /opt/run.sh
CMD ["/bin/sh", "/opt/run.sh"]
