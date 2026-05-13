# syntax=docker/dockerfile:1
#
# Fake GitHub API for the kind smoke (REQ-147 / #335).
# Stdlib-only Python; no pip, no virtualenv.

FROM python:3.12-slim

WORKDIR /app
COPY server.py /app/server.py
EXPOSE 8080
ENV PORT=8080
ENTRYPOINT ["python3", "/app/server.py"]
