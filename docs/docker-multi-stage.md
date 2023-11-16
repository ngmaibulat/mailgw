```Dockerfile
# Build stage
FROM alpine as builder
RUN apk add g++ make
COPY . /src
WORKDIR /src
RUN make

# Production stage
FROM alpine
COPY --from=builder /src/myapp /app/myapp
CMD ["/app/myapp"]
```
