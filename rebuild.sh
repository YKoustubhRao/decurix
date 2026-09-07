docker build -f DesktopDockerfile -t flowera-desktop:latest .
docker compose down
docker ps -a --filter "name=desktop_" --format "{{.ID}} {{.Names}}" | awk '{print $1}' | xargs -r docker rm -f
docker volume ls --filter "name=desktop_" --format "{{.Name}}" | xargs -r docker volume rm
docker compose up --build