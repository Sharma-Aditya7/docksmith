# Docksmith

A lightweight container runtime built in Go, inspired by Docker's core concepts.  
This project explores low-level containerization primitives like namespaces and process isolation.

---

## 🚀 Features

- Container isolation using Linux namespaces  
- Custom runtime execution  
- Image handling via tar layers  
- CLI-based container management  

---

## 🧠 Architecture

```
docksmith/
│── cmd/
│   └── docksmith/
│       └── main.go        # Entry point
│
│── internal/
│   ├── cli/               # CLI parsing and commands
│   ├── runtime/           # Container execution logic
│   ├── build/             # Image creation and handling
│
│── examples/
│   └── myapp/             # Sample containerized app
│
│── assets/                # Base images (tar files)
│── go.mod
```

---

## ⚙️ Installation

### Prerequisites
- Go (>= 1.20 recommended)
- Linux environment (WSL / native Linux)
- Root privileges (for namespace operations)

### Build

```bash
go build -o docksmith ./cmd/docksmith
```

---

## ▶️ Usage

### Run a container

```bash
sudo ./docksmith run myapp:latest
```

### Example

A sample app is available in:

```
examples/myapp/
```

---

## 🔬 How It Works

Docksmith mimics core container runtime behavior:

- Uses **Linux namespaces** for process isolation  
- Executes containers as isolated child processes  
- Handles root filesystem via extracted image layers  
- Provides a CLI interface for interaction  

---

## 📺 Demo

See full walkthrough here: [DEMO.md](./DEMO.md)

---

## 🔮 Future Improvements

- [ ] Cgroups integration (resource limits)  
- [ ] Networking support (bridge, veth pairs)  
- [ ] OverlayFS for layered filesystem  
- [ ] Image pull/push support  
- [ ] Container lifecycle management  

---

## 🧪 Learning Goals

This project was built to understand:

- OS-level virtualization  
- Process isolation mechanisms  
- How Docker works under the hood  
- Systems programming in Go  

---

## 👤 Author

**Aditya Sharma**  

---

## ⭐ Notes

This is a learning-focused project and not intended for production use.
