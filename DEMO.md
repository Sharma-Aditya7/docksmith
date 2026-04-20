# Docksmith Demo Guide

This document provides an end-to-end walkthrough of Docksmith's functionality using the updated repository structure.

---

## 🧱 Setup

```bash
sudo rm -rf /root/.docksmith/
sudo ./docksmith import-base --tar assets/alpine-3.18.tar --name alpine:3.18
```

---

## 📦 Create Sample App

```bash
mkdir -p examples/myapp
echo 'Docksmith container is running!
Built with: FROM COPY RUN WORKDIR ENV CMD' > examples/myapp/message.txt
```

```bash
cat > examples/myapp/Docksmithfile << 'EOF'
FROM alpine:3.18
WORKDIR /app
ENV APP_ENV=production
ENV GREETING=Hello
COPY *.txt /app/
RUN echo "Build complete. GREETING=$GREETING" && ls /app
CMD ["/bin/sh", "-c", "echo $GREETING from Docksmith && cat /app/message.txt"]
EOF
```

---

## ⚙️ Build

### Cold Build
```bash
sudo ./docksmith build -t myapp:latest examples/myapp
```

### Warm Build (Cache Demonstration)
```bash
sudo ./docksmith build -t myapp:latest examples/myapp
```

---

## 🔁 Modify + Rebuild

```bash
echo "changed line" >> examples/myapp/message.txt
sudo ./docksmith build -t myapp:latest examples/myapp
```

---

## 📋 List Images

```bash
sudo ./docksmith images
```

---

## ▶️ Run Container

```bash
sudo ./docksmith run myapp:latest
```

---

## 🌍 Environment Variable Override

```bash
sudo ./docksmith run -e GREETING=Howdy myapp:latest
```

---

## 🔒 Isolation Check

```bash
sudo ./docksmith run myapp:latest "touch /tmp/isolation_sentinel_99"
ls /tmp/isolation_sentinel_99
```

Expected: File should NOT exist on host.

---

## 🧹 Remove Image

```bash
sudo ./docksmith rmi myapp:latest
sudo ./docksmith images
```

---

## ⚠️ Edge Case Tests

### Invalid Instruction
```bash
mkdir -p /tmp/bad_docksmithfile
echo "FROM alpine:3.18
HELLO test" > /tmp/bad_docksmithfile/Docksmithfile
```

### Missing Base Image
```bash
mkdir -p /tmp/from_test
printf 'FROM nonexistent:latest
RUN echo hi' > /tmp/from_test/Docksmithfile
```

### WORKDIR Creation
```bash
mkdir -p /tmp/wd_test
printf 'FROM alpine:3.18
WORKDIR /brandnewdir
RUN pwd' > /tmp/wd_test/Docksmithfile
```

### Recursive COPY (Glob)
```bash
mkdir -p /tmp/globtest/subdir
echo "root file" > /tmp/globtest/root.txt
echo "sub file" > /tmp/globtest/subdir/sub.txt

cat > /tmp/globtest/Docksmithfile << 'EOF'
FROM alpine:3.18
WORKDIR /data
COPY **/*.txt /data/
RUN ls /data
EOF
```

### RUN Isolation Attempt
```bash
mkdir -p /tmp/run_isolation_test
printf 'FROM alpine:3.18
RUN touch /tmp/build_escape_attempt' > /tmp/run_isolation_test/Docksmithfile
```

### Missing CMD
```bash
mkdir -p /tmp/nocmd_test
printf 'FROM alpine:3.18
RUN echo hi' > /tmp/nocmd_test/Docksmithfile
```

### Network Restriction
```bash
mkdir -p /tmp/nettest
cat > /tmp/nettest/Docksmithfile << 'EOF'
FROM alpine:3.18
RUN wget -T 3 http://example.com -O /tmp/test 2>&1 || echo "NETWORK BLOCKED"
EOF
```

---

## 🧠 What This Demonstrates

- Image build lifecycle (cold vs warm builds)
- Layer caching effectiveness
- Environment variable injection
- Process isolation from host
- Handling of invalid configurations
- Basic filesystem operations inside containers
- Network isolation behavior

---

## ⭐ Notes

- Run commands with `sudo` due to namespace operations
- Ensure `docksmith` binary is built using:
```bash
go build -o docksmith ./cmd/docksmith
```
