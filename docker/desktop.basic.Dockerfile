# TheBot's soul binary, built from its real source (agent/thebot/) rather
# than a pre-committed binary - genuinely part of this repo and buildable
# by anyone's own CI, not an opaque blob. Pure-Go (modernc.org/sqlite, no
# CGO) and its whole UI is embedded via go:embed, so the build output is
# one self-contained static binary.
FROM golang:1.27 AS thebot-builder
WORKDIR /src
COPY agent/thebot/go.mod agent/thebot/go.sum ./
RUN go mod download
COPY agent/thebot/cmd ./cmd
COPY agent/thebot/internal ./internal
COPY agent/thebot/web ./web
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /soul ./cmd/soul

FROM accetto/ubuntu-vnc-xfce-chromium-g3:latest

USER root

COPY agent/vm_server.py /opt/vm_server.py
COPY agent/flowera_agent /opt/flowera_agent
COPY agent/start_agent_terminal.sh /opt/start_agent_terminal.sh
# TheBot harness's soul binary - built above from real source, started
# conditionally by start_agent_terminal.sh only when AGENT_HARNESS=thebot.
COPY --from=thebot-builder /soul /opt/soul
# docling_convert.py is only exercised for office-doc RAG ingestion
# (PDF/DOCX/etc - see internal/rag), which needs python3-pip + docling
# installed to actually work; this base image doesn't carry that, so
# those specific formats fail per-file, honestly, without breaking
# anything else - plain text/code files and every other skill (web
# search, terminal, file-write, memory) don't need Python at all.
COPY agent/thebot/scripts /opt/thebot-scripts

RUN chmod +x /opt/vm_server.py /opt/start_agent_terminal.sh /opt/soul

RUN mkdir -p /home/headless/.config/autostart \
    && chown -R headless:headless /home/headless/.config

COPY agent/flowera-agent.desktop /home/headless/.config/autostart/flowera-agent.desktop

RUN chmod 644 /home/headless/.config/autostart/flowera-agent.desktop \
    && chown headless:headless /home/headless/.config/autostart/flowera-agent.desktop

USER headless

ENTRYPOINT ["/usr/bin/tini", "--", "/dockerstartup/startup.sh"]
