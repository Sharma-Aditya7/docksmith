# Docksmith Sample App

This sample uses all six Docksmithfile instructions:

| Instruction | Usage in this sample |
|-------------|---------------------|
| FROM        | alpine:3.18          |
| WORKDIR     | /app                 |
| ENV         | APP_ENV, GREETING    |
| COPY        | . /app               |
| RUN         | echo + ls            |
| CMD         | prints GREETING + message.txt |

## Demo Sequence

```bash
# 0. Get alpine rootfs once
docker export $(docker create alpine:3.18) > alpine-3.18.tar
docksmith import-base --tar alpine-3.18.tar --name alpine:3.18

# 1. Cold build — all CACHE MISS
docksmith build -t myapp:latest .

# 2. Warm build — all CACHE HIT
docksmith build -t myapp:latest .

# 3. Edit message.txt, rebuild — partial cache miss
echo "changed" >> message.txt
docksmith build -t myapp:latest .

# 4. List images
docksmith images

# 5. Run container
docksmith run myapp:latest

# 6. Override env
docksmith run -e GREETING=Howdy myapp:latest

# 7. Isolation check (file must NOT appear on host)
docksmith run myapp:latest "touch /tmp/should_not_appear && echo done"
ls /tmp/should_not_appear  # must fail

# 8. Remove image
docksmith rmi myapp:latest
```
