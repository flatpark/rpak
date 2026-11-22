# A release server shaped like GitHub's, for rpak self-update: BASE/latest
# answers 302 to BASE/tag/TAG, TAG being the content of DIR/latest, and
# everything else is a file under DIR (DIR/releases/download/TAG/NAME).
# Usage: python3 release-server.py DIR PORT   (BASE is /releases)
import functools
import http.server
import os
import sys


class Handler(http.server.SimpleHTTPRequestHandler):
    def latest(self):
        if self.path.rstrip("/") != "/releases/latest":
            return False
        with open(os.path.join(self.directory, "latest")) as f:
            tag = f.read().strip()
        self.send_response(302)
        self.send_header("Location", "/releases/tag/" + tag)
        self.send_header("Content-Length", "0")
        self.end_headers()
        return True

    def do_HEAD(self):
        if not self.latest():
            super().do_HEAD()

    def do_GET(self):
        if not self.latest():
            super().do_GET()


directory, port = sys.argv[1], int(sys.argv[2])
server = http.server.ThreadingHTTPServer(("127.0.0.1", port), functools.partial(Handler, directory=directory))
server.serve_forever()
