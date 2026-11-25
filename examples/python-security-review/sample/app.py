"""A small web service with deliberate security problems, used to try the review."""

import hashlib
import os
import sqlite3
import subprocess

from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse

API_TOKEN = "demo-token-not-a-real-secret"  # hard-coded secret
DB = sqlite3.connect("users.db")


def find_user(name):
    # SQL built from input
    return DB.execute("SELECT id, email FROM users WHERE name = '%s'" % name).fetchall()


def ping(host):
    # shell command built from input
    return subprocess.run("ping -c 1 " + host, shell=True, capture_output=True).stdout


def read_report(name):
    # path from input, never checked
    with open(os.path.join("reports", name)) as f:
        return f.read()


def hash_password(password):
    return hashlib.md5(password.encode()).hexdigest()


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        q = parse_qs(urlparse(self.path).query)
        if self.path.startswith("/user"):
            body = str(find_user(q["name"][0]))
        elif self.path.startswith("/ping"):
            body = ping(q["host"][0]).decode()
        elif self.path.startswith("/report"):
            body = read_report(q["file"][0])
        else:
            body = "ok"
        self.send_response(200)
        self.end_headers()
        self.wfile.write(body.encode())


if __name__ == "__main__":
    HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
