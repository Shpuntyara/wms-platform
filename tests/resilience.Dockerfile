FROM postgres:17.11
RUN apt-get update && apt-get install -y --no-install-recommends python3 && rm -rf /var/lib/apt/lists/*
USER postgres
ENTRYPOINT ["python3", "/repo/scripts/crash_restore.py"]
