"""Broker mode's startup split (agent/decurix-entrypoint.sh, decurix-screen-session.sh, decurix-worker-session.sh;
docs/gate-boundary.md, "Next: the input broker").

These read the scripts, so they run on any host without starting anything: the broker branch runs only with
DECURIX_SCREEN_USER=1 and ends in an exec, the default path after it is the base image's startup as before, the screen
side gets a cleared environment with no secret, the worker gets no screen password, and the screen session gives the
worker only an untrusted cookie.
"""

import re
import unittest
from pathlib import Path

AGENT = Path(__file__).resolve().parents[1]
ENTRYPOINT = AGENT / "decurix-entrypoint.sh"
SCREEN = AGENT / "decurix-screen-session.sh"
WORKER = AGENT / "decurix-worker-session.sh"
DOCKERFILE = AGENT.parent / "docker" / "desktop.basic.Dockerfile"


def code(text):
    return "\n".join(line for line in text.splitlines() if not line.lstrip().startswith("#"))


def broker_block():
    text = code(ENTRYPOINT.read_text(encoding="utf-8"))
    start = text.index('if [ "${DECURIX_SCREEN_USER:-0}" = "1" ]')
    end = text.index("\nfi\n", start)
    return text[start:end], text[end:]


class TheEntrypointsBrokerBranch(unittest.TestCase):
    def setUp(self):
        self.block, self.after = broker_block()

    def test_it_ends_in_the_screen_users_session_and_the_default_path_follows_unchanged(self):
        self.assertTrue(self.block.rstrip().endswith("/usr/bin/env -i \"$@\" /opt/decurix-screen-session.sh"),
                        self.block[-200:])
        self.assertIn("exec /usr/bin/setpriv --reuid=\"$SCREEN_UID\"", self.block)
        # The default mode: the worker runs the base image's startup, as it always did.
        self.assertIn("/dockerstartup/startup.sh", self.after)
        self.assertNotIn("/dockerstartup/startup.sh", self.block)
        self.assertNotIn("decurix-screen-session", self.after)

    def test_every_start_drops_every_capability_and_privilege(self):
        starts = re.findall(r"/usr/bin/setpriv [^\n]*\n[^\n]*", self.block)
        self.assertEqual(len(starts), 3, starts)    # the password file, the worker's session, the screen's session
        for start in starts:
            for flag in ("--inh-caps=-all", "--ambient-caps=-all", "--bounding-set=-all", "--no-new-privs"):
                self.assertIn(flag, start)

    def test_the_password_goes_over_a_pipe_and_then_out_of_the_environment(self):
        self.assertIn('printf \'%s\\n\' "$VNC_PW" | /usr/bin/setpriv', self.block)
        unset = self.block.index("unset VNC_PW VNC_PASSWORD VNC_VIEW_ONLY_PW")
        self.assertLess(unset, self.block.index("/opt/decurix-worker-session.sh"))
        self.assertLess(unset, self.block.index("/opt/decurix-screen-session.sh"))

    def test_the_screen_side_takes_the_desktops_settings_and_no_secret(self):
        skip, take = re.search(r'case "\$name" in\s*\n\s*([^)]*)\) continue ;;\s*\n\s*([^)]*)\) ;;', self.block).groups()
        skipped = skip.split("|")
        for secret in ("DECURIX_AGENT_TOKEN", "DECURIX_GATE_KEY", "VNC_PW", "*_TOKEN", "*_KEY", "*PASSWORD*", "*SECRET*"):
            self.assertIn(secret, skipped)
        self.assertIn("DECURIX_*", take.split("|"))
        self.assertIn("CONTROL_PLANE_URL", take.split("|"))
        self.assertNotIn("PATH", take.split("|"))
        self.assertIn("PATH=/usr/local/bin:/usr/bin:/bin", self.block)

    def test_root_makes_the_x_socket_folder_before_any_user_runs(self):
        self.assertIn("/usr/bin/install -d -m 1777 -o root -g root /tmp/.X11-unix", self.block)
        self.assertLess(self.block.index("/tmp/.X11-unix"), self.block.index("/opt/decurix-worker-session.sh"))


class TheTwoSessions(unittest.TestCase):
    def test_the_screen_runs_the_x_server_with_the_security_extension_and_no_tcp(self):
        screen = code(SCREEN.read_text(encoding="utf-8"))
        self.assertIn("/usr/bin/Xtigervnc :1", screen)
        self.assertIn("-nolisten tcp", screen)
        self.assertIn('-rfbauth "$SCREEN_HOME/vnc.passwd"', screen)
        self.assertIn("decurix_agent.computer.daemon", screen)
        self.assertIn("xfwm4", screen)

    def test_the_worker_is_given_an_untrusted_cookie_and_nothing_else(self):
        screen = code(SCREEN.read_text(encoding="utf-8"))
        generate = re.findall(r"xauth[^\n]*generate[^\n]*", screen)
        self.assertEqual(len(generate), 1, generate)
        self.assertIn(":1 . untrusted", generate[0])
        self.assertIn('"$SHARED/worker.xauth.new"', generate[0])
        # The trusted cookie stays in the screen user's own folder, made with umask 077.
        self.assertIn('XAUTHORITY="$SCREEN_HOME/Xauthority"', screen)
        self.assertIn("umask 077", screen)
        self.assertNotIn("xhost", screen)

    def test_the_worker_session_starts_its_parts_and_the_agent_but_no_x_server_or_window_manager(self):
        worker = code(WORKER.read_text(encoding="utf-8"))
        self.assertIn("/opt/start_agent_terminal.sh", worker)
        for part in ("xfce4-panel", "xfsettingsd"):
            self.assertIn(part, worker)
        # Not xfdesktop: as an untrusted client it cannot run, and as the screen user its menus would start programs
        # past the gate; the screen user draws the wallpaper (computer/wallpaper.py).
        self.assertNotIn("xfdesktop", worker)
        for screen_part in ("Xtigervnc", "Xvnc", "vncserver", "xfwm4", "chromium-browser", "xhost"):
            self.assertNotIn(screen_part, worker)
        self.assertIn('XAUTHORITY="$HOME/.Xauthority"', worker)

    def test_the_agent_start_does_not_touch_x_access_in_broker_mode(self):
        start = (AGENT / "start_agent_terminal.sh").read_text(encoding="utf-8")
        line = next(i for i, l in enumerate(start.splitlines()) if "xhost -" in l)
        guard = "\n".join(start.splitlines()[max(0, line - 3):line])
        self.assertIn('"${DECURIX_SCREEN_USER:-}" != "1"', guard)

    @unittest.skipUnless(DOCKERFILE.is_file(), "the repository's docker/ folder is not beside this agent/ copy")
    def test_the_image_has_both_sessions_and_keeps_the_default_modes_vnc(self):
        dockerfile = DOCKERFILE.read_text(encoding="utf-8")
        for path in ("/opt/decurix-screen-session.sh", "/opt/decurix-worker-session.sh",
                     "/etc/xdg/decurix-screen/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml"):
            self.assertIn(path, dockerfile)
        self.assertIn("tigervnc-standalone-server", dockerfile)
        self.assertIn("ln -sfn /usr/libexec/vncserver /usr/bin/vncserver", dockerfile)
        self.assertIn("--force-confold", dockerfile)


if __name__ == "__main__":
    unittest.main()
