# Flowera

A web-based desktop management system that uses Docker to create, manage, and monitor isolated desktop environments, with an integrated Ollama-backed AI agent running inside each one.

## Architecture

This project is a system for managing desktop environments within a Docker-based infrastructure. It provides a web-based interface for users to create, manage, and interact with desktop containers, which are configured based on predefined profiles (basic, developer, or AI). The system ensures that necessary Docker images and networks are available, pulling them if they are not already present on the host machine. When a desktop is created, it sets up a persistent home volume, configures the container with appropriate environment variables, resource limits, and network settings, and starts the container. The system maintains a list of active desktops and provides endpoints to start, stop, restart, or delete them as needed.

Communication between the user interface and the desktop containers is handled through HTTP endpoints, allowing messages to be sent to a running desktop or for the system to receive help notes from users. These notes are stored and can be viewed on a dashboard, which also displays information about active desktops and their status. The system includes support for an Ollama service, which is managed to ensure it is running and that specific models are available. A reverse proxy is used to route traffic to the appropriate desktop containers, ensuring that users can access their desktop environments seamlessly through the network. The architecture is designed to be modular, with each component handling a specific responsibility, from container management to user interaction and service orchestration.

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
