FROM accetto/ubuntu-vnc-xfce-chromium-g3:latest

USER root

COPY agent/vm_server.py /opt/vm_server.py
COPY agent/flowera_agent /opt/flowera_agent
COPY agent/start_agent_terminal.sh /opt/start_agent_terminal.sh

RUN chmod +x /opt/vm_server.py /opt/start_agent_terminal.sh

RUN mkdir -p /home/headless/.config/autostart \
    && chown -R headless:headless /home/headless/.config

COPY agent/flowera-agent.desktop /home/headless/.config/autostart/flowera-agent.desktop

RUN chmod 644 /home/headless/.config/autostart/flowera-agent.desktop \
    && chown headless:headless /home/headless/.config/autostart/flowera-agent.desktop

USER headless

ENTRYPOINT ["/usr/bin/tini", "--", "/dockerstartup/startup.sh"]