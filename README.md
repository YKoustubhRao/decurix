# Flowera

A web-based desktop management system that uses Docker to create, manage, and monitor isolated desktop environments, with an integrated Ollama-backed AI agent running inside each one.

## Architecture

This project is a web-based desktop management system that leverages Docker to create, manage, and monitor isolated desktop environments. At its core, the system provides RESTful HTTP endpoints for starting, stopping, restarting, and deleting desktop sessions, which are implemented as Docker containers. Each desktop is associated with a unique ID and is configured based on a profile (basic, developer, or AI), ensuring that resources such as CPU, memory, and persistent storage are appropriately allocated.

The system maintains a list of active desktops by querying Docker for containers with a specific label, allowing it to dynamically track the state and metadata of each session. It also includes functionality for sending messages to running desktops, managing help notes, and ensuring that necessary Docker images and networks are available. A reverse proxy is used to route incoming HTTP traffic to the appropriate desktop container, enabling seamless access to desktop applications running inside the Docker environment.

Supporting infrastructure is managed through helper functions that ensure required Docker images are pulled, networks are created, and services like Ollama are properly initialized and monitored. All user interactions are handled through a web dashboard, which displays desktop status, help notes, and other relevant information, providing a centralized interface for managing the desktop environment.

## Component Reference

See [CODE_MAP.md](CODE_MAP.md) for the full per-function index - every entry is algorithmically extracted (not hand-written) and cites its exact file and line range.

## Usage

```bash
./build.sh              # normal cached build
./build.sh --no-cache   # force rebuild
```

## Issues

1. Docker build (./build.sh) is very slow + heavy. Optimize
2. Codebase reorg, put to folders and vreak the big main.go and vm_server.py
3. OpenClaw and other harnesses integration
4. AI bot assistant to handle dashboard management (like create multiple tickets, resolve basic permissions n doubts)
5. Documentation reader and internal searchable index over the document corpus
6. Make the UI much better
7. Have a desktop app
8. Update this README.md as well
